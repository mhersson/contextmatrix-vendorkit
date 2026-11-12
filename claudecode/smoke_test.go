package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The smoke tests run the real claude binary through Run and Session against the mock
// Messages API in testdata/probe/scripts/mock.py (scenario smoke), with dummy
// credentials only; testdata/probe/scripts/smoke.sh runs them in the jail.

func smokeTarget(t *testing.T) (binary, mockURL string) {
	t.Helper()

	binary, mockURL = os.Getenv("VENDORKIT_SMOKE_CLAUDE"), os.Getenv("VENDORKIT_SMOKE_MOCK")
	if binary == "" || mockURL == "" {
		t.Skip("set VENDORKIT_SMOKE_CLAUDE and VENDORKIT_SMOKE_MOCK to run claude against a mock API")
	}

	return binary, mockURL
}

// smokeRun returns a Spec for the real binary and run options that point it
// at the mock and capture the raw stream.
func smokeRun(t *testing.T, binary, mockURL string) (Spec, runOpts, *streamLog) {
	t.Helper()

	stream := &streamLog{}
	opts := runOpts{
		gateTimeout: gateTimeout,
		grace:       stopGrace,
		extraEnv:    []string{"ANTHROPIC_BASE_URL=" + mockURL},
		onLine:      stream.add,
	}

	return Spec{
		Binary:     binary,
		Dir:        t.TempDir(),
		Home:       t.TempDir(),
		OAuthToken: "dummy-oauth-token",
		Env:        []string{"PATH=/usr/bin:/bin"},
		Model:      "claude-haiku-5-5",
		Emitter:    events.NewEmitter(nil, newEventLog()),
	}, opts, stream
}

type streamLog struct {
	mu    sync.Mutex
	lines [][]byte
}

func (s *streamLog) add(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lines = append(s.lines, bytes.Clone(line))
}

// last returns the last line of type typ (and subtype, when not empty), decoded.
func (s *streamLog) last(t *testing.T, typ, subtype string) map[string]any {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.lines) - 1; i >= 0; i-- {
		var l map[string]any
		if json.Unmarshal(s.lines[i], &l) == nil && l["type"] == typ && (subtype == "" || l["subtype"] == subtype) {
			return l
		}
	}

	t.Fatalf("no %s %s line in the stream", typ, subtype)

	return nil
}

// TestSmokeAgainstMockAPI proves the generated mcp.json and settings.json on
// the real CLI: a refused finish keeps the run going, an accepted one ends it
// through the http stop hook. With a dead proxy in the env, the loopback mock,
// bridge and hook must bypass it.
func TestSmokeAgainstMockAPI(t *testing.T) {
	binary, mockURL := smokeTarget(t)

	// Port 9 has no listener inside the jail.
	const dead = "http://127.0.0.1:9"

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"direct", nil},
		{"dead proxy", []string{"HTTP_PROXY=" + dead, "HTTPS_PROXY=" + dead, "ALL_PROXY=" + dead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, opts, stream := smokeRun(t, binary, mockURL)
			spec.Env = append(spec.Env, tc.env...)
			spec.Task = "Finish the task."
			spec.Tools = tools.NewRegistry(stubTool{name: "finish", terminal: true, exec: func(args map[string]any) (tools.Result, error) {
				if s, _ := args["commit_message"].(string); s == "" {
					return tools.Result{}, errors.New("commit_message is required")
				}

				return tools.Result{Text: `{"accepted":true}`}, nil
			}})

			res, err := run(t.Context(), spec, opts)
			require.NoError(t, err)

			result := stream.last(t, "result", "")
			t.Logf("init mcp_servers %v; result terminal_reason %v, num_turns %v",
				stream.last(t, "system", "init")["mcp_servers"], result["terminal_reason"], result["num_turns"])

			assert.Equal(t, "hook_stopped", result["terminal_reason"])
			assert.True(t, res.Completed)
			assert.Equal(t, "done", res.Reason)
			assert.Equal(t, 1, res.ToolCallFailures)
			assert.JSONEq(t, `{"commit_message":"feat: smoke","summary":"done"}`, string(res.CompletionArgs))
		})
	}
}

// TestSmokeBridgedResultWhole proves a bridged result over the CLI's default
// persist threshold reaches the API whole: the mock reports the length and tail
// of the tool_result it received.
func TestSmokeBridgedResultWhole(t *testing.T) {
	binary, mockURL := smokeTarget(t)

	const marker = "BIG-END-7f3a"

	spec, opts, _ := smokeRun(t, binary, mockURL)
	spec.Task = "Call big."
	spec.Tools = tools.NewRegistry(stubTool{name: "big", exec: func(map[string]any) (tools.Result, error) {
		return tools.Result{Text: strings.Repeat("a", 60_000) + marker}, nil
	}})

	res, err := run(t.Context(), spec, opts)
	require.NoError(t, err)

	var got struct {
		Len  int    `json:"len"`
		Tail string `json:"tail"`
	}

	require.NoError(t, json.Unmarshal([]byte(res.Output), &got), res.Output)
	assert.Equal(t, 60_000+len(marker), got.Len)
	assert.True(t, strings.HasSuffix(got.Tail, marker), got.Tail)
}

// TestSmokeNoAttribution proves Run and Session start claude with its commit
// and PR attribution off, also when Spec.Env names a remote session (which adds
// a Claude-Session line): the mock answers with the attribution texts it found
// in the request that carried the task.
func TestSmokeNoAttribution(t *testing.T) {
	binary, mockURL := smokeTarget(t)

	const task = "Commit the change. REPORT-TASK-7f3a"

	remote := []string{"CLAUDE_CODE_ENTRYPOINT=remote", "CLAUDE_CODE_REMOTE_SESSION_ID=session_01SmokeProbe"}

	for _, tc := range []struct {
		name    string
		session bool
		env     []string
	}{
		{"run", false, nil},
		{"session", true, nil},
		{"run remote session env", false, remote},
		{"session remote session env", true, remote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, opts, _ := smokeRun(t, binary, mockURL)
			spec.Env = append(spec.Env, tc.env...)
			// 2.1.322 sends its attribution guidance only when Bash is available.
			spec.BuiltinTools = []string{"Bash"}
			spec.Tools = tools.NewRegistry(stubTool{name: "report", exec: func(map[string]any) (tools.Result, error) {
				return tools.Result{}, errors.New("the mock answers report requests without calling it")
			}})

			var res harness.Result

			if tc.session {
				s, err := startSession(t.Context(), spec, opts)
				require.NoError(t, err)
				t.Cleanup(func() { _ = s.Close() })

				res, err = s.Send(t.Context(), task, nil)
				require.NoError(t, err)
			} else {
				spec.Task = task

				var err error

				res, err = run(t.Context(), spec, opts)
				require.NoError(t, err)
			}

			var got struct {
				Attribution map[string][]string `json:"attribution"`
				Task        bool                `json:"task"`
			}

			require.NoError(t, json.Unmarshal([]byte(res.Output), &got), res.Output)
			require.True(t, got.Task, "the mock did not see the task in the request it reported on")
			require.Len(t, got.Attribution, 3, "system, tools and messages")

			for part, found := range got.Attribution {
				assert.Empty(t, found, part)
			}
		})
	}
}

// TestSmokeCancelKillsBashChild cancels a run while the Bash built-in waits on
// a long-lived child and checks that the child goes with the run.
func TestSmokeCancelKillsBashChild(t *testing.T) {
	smokeCancelBash(t, stopGrace)
}

// TestSmokeCancelKillsBashChildOnKill leaves claude no time to clean up: the
// Bash child sits in its own session, so only the descendant kill reaches it.
func TestSmokeCancelKillsBashChildOnKill(t *testing.T) {
	smokeCancelBash(t, time.Millisecond)
}

func smokeCancelBash(t *testing.T, grace time.Duration) {
	t.Helper()

	binary, mockURL := smokeTarget(t)
	spec, opts, stream := smokeRun(t, binary, mockURL)
	opts.grace = grace
	spec.Task = "Run the command."
	spec.BuiltinTools = []string{"Bash"}

	// mock.py's smoke scenario has Bash write this file in the run's cwd.
	pidPath := filepath.Join(spec.Dir, "bash.pid")

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	type sighting struct {
		at      time.Time
		pid     int
		alive   bool
		pgrp    string
		cmdline string
	}

	cause := errors.New("cost cap")
	seen := make(chan sighting, 1)

	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				data, err := os.ReadFile(pidPath)
				if err != nil || !bytes.HasSuffix(data, []byte("\n")) {
					continue
				}

				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))

				seen <- sighting{time.Now(), pid, !processGone(pid), procGroup(pid), string(bytes.ReplaceAll(cmdline, []byte{0}, []byte(" ")))}

				cancel(cause)

				return
			}
		}
	}()

	res, err := run(ctx, spec, opts)
	returned := time.Now()

	require.ErrorIs(t, err, cause)
	assert.Equal(t, "canceled", res.Reason)

	var s sighting

	select {
	case s = <-seen:
	default:
		t.Fatal("the run ended before the Bash child wrote its PID")
	}

	t.Cleanup(func() {
		if !processGone(s.pid) {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
	})

	gate := stream.last(t, "control_response", "")
	claudePID := gate["response"].(map[string]any)["response"].(map[string]any)["pid"]
	t.Logf("claude pid %v; at cancel the Bash child %d (%q) alive=%v in process group %s; Run returned %s after cancel",
		claudePID, s.pid, strings.TrimSpace(s.cmdline), s.alive, s.pgrp, returned.Sub(s.at).Round(time.Millisecond))

	require.True(t, s.alive, "the Bash child was gone before the cancel")
	assert.Less(t, returned.Sub(s.at), 3*time.Second)
	assert.Eventually(t, func() bool { return processGone(s.pid) }, 2*time.Second, 20*time.Millisecond,
		"the Bash tool's child %d outlived the run", s.pid)
}

// procGroup is the process group /proc reports for pid; "" when unknown.
func procGroup(pid int) string {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}

	_, after, _ := bytes.Cut(stat, []byte(") "))
	if fields := strings.Fields(string(after)); len(fields) > 2 {
		return fields[2]
	}

	return ""
}
