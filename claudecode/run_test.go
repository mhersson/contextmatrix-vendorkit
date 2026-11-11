package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-vendorkit/internal/fakeclaude"
	"github.com/mhersson/contextmatrix-vendorkit/subproc"
)

const fakeToken = "test-oauth-token"

// eventLog is an Emitter transcript that signals its first event.
type eventLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	first chan struct{}
	once  sync.Once
}

func newEventLog() *eventLog { return &eventLog{first: make(chan struct{})} }

func (l *eventLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.once.Do(func() { close(l.first) })

	return l.buf.Write(p)
}

func (l *eventLog) raw() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()

	return bytes.Clone(l.buf.Bytes())
}

func (l *eventLog) events(t *testing.T) []events.Event {
	t.Helper()

	return decodeEvents(t, l.raw())
}

func writeScenario(t *testing.T, sc fakeclaude.Scenario) string {
	t.Helper()

	b, err := json.Marshal(sc)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "scenario.json")
	require.NoError(t, os.WriteFile(path, b, 0o600))

	return path
}

// fixture is the absolute path of a testdata file; the fake runs in Spec.Dir.
func fixture(t *testing.T, rel string) string {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("testdata", rel))
	require.NoError(t, err)

	return path
}

// tokenFixture writes a copy of real/t1.jsonl whose text and result carry the
// token, with the result marked as an error.
func tokenFixture(t *testing.T) string {
	t.Helper()

	data := rewrite(readFixture(t, "real/t1.jsonl"),
		`"text":"ok"`, `"text":"ok `+fakeToken+`"`,
		`"result":"ok"`, `"result":"leaked `+fakeToken+`"`,
		`"is_error":false`, `"is_error":true`)
	require.Contains(t, string(data), fakeToken)

	path := filepath.Join(t.TempDir(), "token.jsonl")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

type fakeRun struct {
	spec   Spec
	log    *eventLog
	record string
}

// newFakeRun returns a Spec that runs the fake claude with sc.
func newFakeRun(t *testing.T, sc fakeclaude.Scenario) *fakeRun {
	t.Helper()

	if sc.Record == "" {
		sc.Record = filepath.Join(t.TempDir(), "record.json")
	}

	if sc.Version == "" {
		sc.Version = Version
	}

	log := newEventLog()

	return &fakeRun{
		spec: Spec{
			Binary:     os.Args[0],
			Dir:        t.TempDir(),
			Home:       t.TempDir(),
			OAuthToken: fakeToken,
			Env:        []string{fakeclaude.EnvVar + "=" + writeScenario(t, sc)},
			Model:      "claude-haiku-5-5",
			Task:       "say ok",
			Emitter:    events.NewEmitter(nil, log),
		},
		log:    log,
		record: sc.Record,
	}
}

func (f *fakeRun) recorded(t *testing.T) fakeclaude.Recorded {
	t.Helper()

	data, err := os.ReadFile(f.record)
	require.NoError(t, err)

	var rec fakeclaude.Recorded
	require.NoError(t, json.Unmarshal(data, &rec))

	return rec
}

type stubTool struct {
	name     string
	terminal bool
	exec     func(args map[string]any) (tools.Result, error)
}

func (s stubTool) Name() string   { return s.name }
func (s stubTool) Terminal() bool { return s.terminal }

func (s stubTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: s.name, Description: s.name, Parameters: json.RawMessage(`{"type":"object"}`),
	}}
}

func (s stubTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	return s.exec(args)
}

// testTools holds a terminal finish that refuses a call without
// commit_message, and an echo of its text argument.
func testTools() *tools.Registry {
	return tools.NewRegistry(
		stubTool{name: "echo", exec: func(args map[string]any) (tools.Result, error) {
			s, _ := args["text"].(string)

			return tools.Result{Text: s}, nil
		}},
		stubTool{name: "finish", terminal: true, exec: func(args map[string]any) (tools.Result, error) {
			if s, _ := args["commit_message"].(string); s == "" {
				return tools.Result{}, errors.New("commit_message is required")
			}

			return tools.Result{Text: `{"accepted":true}`}, nil
		}},
	)
}

func requireInvocationInvariants(t *testing.T, args []string) {
	t.Helper()

	for flag, want := range map[string]string{
		"--output-format": "stream-json", "--input-format": "stream-json", "--setting-sources": "user",
		"--permission-mode": "dontAsk", "--system-prompt-snapshot": "off",
	} {
		require.Equal(t, want, flagValue(t, args, flag), flag)
	}

	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages", "--strict-mcp-config", "--model", "--mcp-config", "--settings", "--tools"} {
		require.Contains(t, args, flag)
	}

	require.NotContains(t, args, "--bare")
}

func requireNoRunDirs(t *testing.T, home string) {
	t.Helper()

	dirs, err := filepath.Glob(filepath.Join(home, ".vendorkit", "run-*"))
	require.NoError(t, err)
	require.Empty(t, dirs)
}

// processGone reports whether pid no longer runs; an unreaped zombie counts.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}

	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}

	_, after, ok := bytes.Cut(stat, []byte(") "))

	return ok && len(after) > 0 && after[0] == 'Z'
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)

	return pid
}

func TestRunBasic(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	f.spec.Env = append(f.spec.Env, "ANTHROPIC_API_KEY=stray", "ANTHROPIC_BASE_URL=http://127.0.0.1:1")

	res, err := Run(t.Context(), f.spec)
	require.NoError(t, err)

	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 1, res.Turns)
	assert.Equal(t, "ok", res.Output)
	assert.Equal(t, "claude-haiku-5-5", res.ModelUsed)
	assert.Equal(t, buckets{2, 4, 11382, 6379}, resultBuckets(res))
	assert.InDelta(t, 0.0013918200000000002, res.TotalCostUSD, 1e-12)
	assert.Nil(t, res.CompletionArgs)

	evs := f.log.events(t)
	assert.Equal(t, []events.Kind{events.ModelRequest, events.ModelResponse, events.UsageKind, events.StateChange}, kinds(evs))
	assert.Equal(t, map[string]any{"stop": "done", "turns": 1.0}, evs[len(evs)-1].Data)

	rec := f.recorded(t)
	requireInvocationInvariants(t, rec.Args)
	assert.Contains(t, rec.Args, "--no-session-persistence")

	want, err := userMessage(f.spec.Task, nil)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), rec.Stdin)
	assert.False(t, rec.EarlyInput)

	for _, name := range rec.EnvNames {
		assert.False(t, strings.HasPrefix(name, "ANTHROPIC_"), name)
	}

	assert.True(t, rec.TokenSet)
	assert.NoDirExists(t, filepath.Join(f.spec.Home, ".vendorkit", "sessions"))
}

func TestRunTerminalStops(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		Steps:  []fakeclaude.Step{{Tool: "finish", Args: map[string]any{"commit_message": "c", "summary": "s"}}},
		Replay: fixture(t, "real/t1.jsonl"),
	})
	f.spec.Tools = testTools()

	stream := &streamLog{}

	res, err := run(t.Context(), f.spec, runOpts{gateTimeout: gateTimeout, grace: stopGrace, onLine: stream.add})
	require.NoError(t, err)

	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.JSONEq(t, `{"commit_message":"c","summary":"s"}`, string(res.CompletionArgs))

	// The stop hook ended the run, not the post-terminal backstop.
	assert.Equal(t, "hook_stopped", stream.last(t, "result", "")["terminal_reason"])

	// The replay would have added its own call and the output "ok".
	assert.Empty(t, res.Output)
	assert.Len(t, ofKind(f.log.events(t), events.ModelRequest), 1)

	var settings settingsFile
	require.NoError(t, json.Unmarshal([]byte(f.recorded(t).RunFiles["--settings"]), &settings))
	require.Len(t, settings.Hooks["PostToolUse"], 1)
	assert.Equal(t, "http", settings.Hooks["PostToolUse"][0].Hooks[0].Type)
}

func TestRunTerminalRefusedKeepsGoing(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Steps: []fakeclaude.Step{
		{Tool: "finish", Args: map[string]any{}},
		{Tool: "finish", Args: map[string]any{"commit_message": "c"}},
	}})
	f.spec.Tools = testTools()

	res, err := Run(t.Context(), f.spec)
	require.NoError(t, err)

	evs := f.log.events(t)
	results := ofKind(evs, events.ToolResult)
	require.Len(t, results, 2)
	assert.Contains(t, results[0].Data, "error")
	assert.NotContains(t, results[1].Data, "error")

	require.Len(t, stops(evs), 1)
	assert.Equal(t, map[string]any{"stop": "done", "turns": 2.0}, evs[len(evs)-1].Data)

	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 1, res.ToolCallFailures)
	assert.JSONEq(t, `{"commit_message":"c"}`, string(res.CompletionArgs))
}

func TestRunHookOnErrorStillGated(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		HookOnError: true,
		Steps:       []fakeclaude.Step{{Tool: "finish", Args: map[string]any{}}},
		Replay:      fixture(t, "real/t1.jsonl"),
	})
	f.spec.Tools = testTools()

	res, err := Run(t.Context(), f.spec)
	require.NoError(t, err)

	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, "ok", res.Output, "the refused call stopped the run")
	assert.Nil(t, res.CompletionArgs)
}

func TestRunMasksTokenEverywhere(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		Steps:    []fakeclaude.Step{{Tool: "echo", Args: map[string]any{"text": "env says " + fakeToken}}},
		Replay:   tokenFixture(t),
		Stderr:   "fatal: bad credentials " + fakeToken + "\n",
		ExitCode: 1,
	})
	f.spec.Tools = testTools()
	f.spec.SessionID = "0b9d3c5e-7a41-4f2e-9c8b-1d2e3f4a5b6c"

	res, err := Run(t.Context(), f.spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fatal: bad credentials", "the stderr tail is missing from the error")
	assert.NotContains(t, err.Error(), fakeToken)

	assert.Contains(t, string(f.log.raw()), "env says", "the tool output never reached the events")
	assert.NotContains(t, string(f.log.raw()), fakeToken)

	out, jerr := json.Marshal(res)
	require.NoError(t, jerr)
	assert.Contains(t, string(out), "leaked", "the result text never reached the Result")
	assert.NotContains(t, string(out), fakeToken)

	rec := f.recorded(t)
	require.NotEmpty(t, rec.RunFiles["--mcp-config"])

	for flag, content := range rec.RunFiles {
		assert.NotContains(t, content, fakeToken, flag)
	}

	home, err := os.OpenRoot(f.spec.Home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = home.Close() })

	stored := 0

	require.NoError(t, fs.WalkDir(home.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := fs.ReadFile(home.FS(), path)
		require.NoError(t, err)
		assert.NotContains(t, string(data), fakeToken, path)

		stored++

		return nil
	}))
	assert.Positive(t, stored, "the run stored no transcript to check")
}

func TestRunCancelKillsProcessGroup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "grandchild.pid")
	f := newFakeRun(t, fakeclaude.Scenario{
		Replay: fixture(t, "synthetic/crash.jsonl"), Hang: true, Grandchild: pidPath,
	})

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	cause := errors.New("cost cap")
	canceledAt := make(chan time.Time, 1)

	go func() {
		select {
		case <-f.log.first:
			canceledAt <- time.Now()

			cancel(cause)
		case <-ctx.Done():
		}
	}()

	res, err := Run(ctx, f.spec)
	returned := time.Now()

	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, "canceled", res.Reason)
	assert.Less(t, returned.Sub(<-canceledAt), 3*time.Second)

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "canceled", evs[len(evs)-1].Data["stop"])

	grandchild := readPID(t, pidPath)
	assert.Eventually(t, func() bool { return processGone(grandchild) }, 2*time.Second, 20*time.Millisecond,
		"grandchild %d outlived the run", grandchild)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)
}

func TestRunCrashWithoutResult(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{
		Replay: fixture(t, "synthetic/crash.jsonl"), Stderr: "boom", ExitCode: 137,
	})

	res, err := Run(t.Context(), f.spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Equal(t, "error", res.Reason)
	assert.False(t, res.Completed)

	_, want, _ := replayData(t, readFixture(t, "synthetic/crash.jsonl"),
		mapperOpts{Prefix: "mcp__cm__", Task: f.spec.Task}, errors.New("exit status 137"))
	assert.NotZero(t, resultBuckets(res))
	assert.Equal(t, resultBuckets(want), resultBuckets(res))
	assert.Equal(t, roles(want.Messages), roles(res.Messages))
	assert.Equal(t, want.ToolCallCount, res.ToolCallCount)
}

func TestRunUsageLimit(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "synthetic/rejected.jsonl"), Hang: true})

	start := time.Now()
	res, err := Run(t.Context(), f.spec)

	assert.Less(t, time.Since(start), 3*time.Second)

	var ule *UsageLimitError
	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "seven_day", ule.Window)
	assert.Equal(t, "error", res.Reason)
	assert.True(t, processGone(f.recorded(t).PID))
}

func TestRunWaitsForInitialize(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), InitDelay: 300 * time.Millisecond})

	_, err := Run(boundCtx(t), f.spec)
	require.NoError(t, err)
	assert.False(t, f.recorded(t).EarlyInput, "the task was written before claude answered initialize")
}

func TestRunInitializeTimeout(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{NoInit: true})

	start := time.Now()
	res, err := run(t.Context(), f.spec, runOpts{gateTimeout: 300 * time.Millisecond, grace: stopGrace})

	require.Error(t, err)
	assert.Equal(t, "error", res.Reason)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)
}

// foreignAccounts are initialize answers whose run would not bill the
// subscription token: 2.1.322's shapes under a provider switch, an API key
// alone and an API key beside the token.
var foreignAccounts = map[string]string{
	"provider switch": `{"apiProvider":"bedrock"}`,
	"api key only":    `{"tokenSource":"none","apiKeySource":"ANTHROPIC_API_KEY","apiProvider":"firstParty"}`,
	"api key wins":    `{"tokenSource":"CLAUDE_CODE_OAUTH_TOKEN","apiKeySource":"ANTHROPIC_API_KEY","apiProvider":"firstParty"}`,
	"no token":        `{"tokenSource":"none","apiProvider":"firstParty"}`,
}

func TestRunFailsClosedOnForeignAccount(t *testing.T) {
	for name, acct := range foreignAccounts {
		t.Run(name, func(t *testing.T) {
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), Account: json.RawMessage(acct)})

			res, err := Run(boundCtx(t), f.spec)

			require.ErrorIs(t, err, errNotSubscription)
			assert.Equal(t, "error", res.Reason)
			assert.False(t, res.Completed)

			var seen map[string]string
			require.NoError(t, json.Unmarshal([]byte(acct), &seen))

			for _, v := range seen {
				assert.Contains(t, err.Error(), strconv.Quote(v), "the error names what claude reported")
			}

			rec := f.recorded(t)
			assert.Empty(t, rec.Stdin, "the task reached a process that would not bill the subscription")
			assert.True(t, processGone(rec.PID))
			requireNoRunDirs(t, f.spec.Home)

			evs := f.log.events(t)
			assert.Empty(t, ofKind(evs, events.ModelRequest))
			require.Len(t, ofKind(evs, events.ErrorKind), 1)
			require.Len(t, stops(evs), 1)
			assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])
		})
	}
}

func TestRunProceedsWithoutAccount(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), NoAccount: true})

	res, err := Run(boundCtx(t), f.spec)
	require.NoError(t, err)
	assert.Equal(t, "done", res.Reason)
}

func TestRunCleansRunDir(t *testing.T) {
	for name, sc := range map[string]fakeclaude.Scenario{
		"done":  {Replay: "real/t1.jsonl"},
		"crash": {Replay: "synthetic/crash.jsonl", ExitCode: 1},
	} {
		t.Run(name, func(t *testing.T) {
			sc.Replay = fixture(t, sc.Replay)
			f := newFakeRun(t, sc)
			f.spec.SessionID = "5d0c6c1e-3f7a-4b2d-8e9f-0a1b2c3d4e5f"

			keep := filepath.Join(f.spec.Home, "keep.txt")
			require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o600))

			_, _ = Run(t.Context(), f.spec)

			requireNoRunDirs(t, f.spec.Home)
			assert.FileExists(t, keep)
			assert.FileExists(t, filepath.Join(f.spec.Home, ".vendorkit", "sessions",
				fakeclaude.ProjectKey(evalDir(t, f.spec.Dir)), f.spec.SessionID+".jsonl"))
		})
	}
}

func TestRunConcurrentSharedHome(t *testing.T) {
	home := t.TempDir()
	runs := make([]*fakeRun, 2)

	for i := range runs {
		runs[i] = newFakeRun(t, fakeclaude.Scenario{
			Steps: []fakeclaude.Step{{Tool: "finish", Args: map[string]any{"commit_message": strconv.Itoa(i)}}},
		})
		runs[i].spec.Home = home
		runs[i].spec.Tools = testTools()
	}

	var wg sync.WaitGroup

	errs := make([]error, len(runs))
	args := make([]string, len(runs))

	for i, f := range runs {
		wg.Go(func() {
			res, err := Run(t.Context(), f.spec)
			errs[i], args[i] = err, string(res.CompletionArgs)
		})
	}

	wg.Wait()

	for i := range runs {
		require.NoError(t, errs[i])
		assert.JSONEq(t, fmt.Sprintf(`{"commit_message":%q}`, strconv.Itoa(i)), args[i])
	}

	a, b := runs[0].recorded(t), runs[1].recorded(t)
	assert.NotEqual(t, flagValue(t, a.Args, "--mcp-config"), flagValue(t, b.Args, "--mcp-config"))
	assert.NotEqual(t, flagValue(t, a.Args, "--settings"), flagValue(t, b.Args, "--settings"))
	assert.NotEqual(t, a.ConfigDir, b.ConfigDir)
	requireNoRunDirs(t, home)
}

func TestRunFreshConfigDir(t *testing.T) {
	f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})

	userDir := filepath.Join(f.spec.Home, ".claude")
	require.NoError(t, os.MkdirAll(userDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(userDir, "settings.json"), []byte(`{"hooks":{}}`), 0o600))

	_, err := Run(t.Context(), f.spec)
	require.NoError(t, err)

	rec := f.recorded(t)
	assert.Empty(t, rec.ConfigFiles)
	assert.False(t, strings.HasPrefix(rec.ConfigDir, userDir), rec.ConfigDir)
	assert.True(t, strings.HasPrefix(rec.ConfigDir, filepath.Join(f.spec.Home, ".vendorkit", "run-")), rec.ConfigDir)
}

// evalDir is dir as the child's getcwd reports it.
func evalDir(t *testing.T, dir string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	return resolved
}

func TestRunResumeCarriesOnlyTranscript(t *testing.T) {
	const sid = "6f1c2a9e-0d3b-4c51-9a7e-2b8f4d6e1a30"

	home, dir := t.TempDir(), t.TempDir()

	a := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl"), Plant: true})
	a.spec.Home, a.spec.Dir, a.spec.SessionID, a.spec.Task = home, dir, sid, "TASK-A"

	_, err := Run(t.Context(), a.spec)
	require.NoError(t, err)
	assert.Equal(t, sid, flagValue(t, a.recorded(t).Args, "--session-id"))
	assert.NotContains(t, a.recorded(t).Args, "--no-session-persistence")

	b := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	b.spec.Home, b.spec.Dir, b.spec.Resume, b.spec.Task = home, dir, sid, "TASK-B"

	_, err = Run(t.Context(), b.spec)
	require.NoError(t, err)

	key := fakeclaude.ProjectKey(evalDir(t, dir))
	rec := b.recorded(t)
	assert.Equal(t, sid, flagValue(t, rec.Args, "--resume"))
	assert.Equal(t, []string{"projects/" + key + "/" + sid + ".jsonl"}, rec.ConfigFiles)

	stored, err := os.ReadFile(filepath.Join(home, ".vendorkit", "sessions", key, sid+".jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(stored), "TASK-A", "B did not start from A's transcript")
	assert.Contains(t, string(stored), "TASK-B")

	entries, err := os.ReadDir(filepath.Join(home, ".vendorkit"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "sessions", entries[0].Name())

	c := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
	c.spec.Home, c.spec.Dir, c.spec.Resume = home, dir, "no-such-session"

	_, err = Run(t.Context(), c.spec)
	require.Error(t, err)
	assert.NoFileExists(t, c.record, "the process started without a transcript to resume")
}

func TestStderrDetailDropsCutToken(t *testing.T) {
	mask := newMask(fakeToken, nil)

	// A full tail was cut at the cap and can open with the token's end.
	body := " trailing\nnext " + fakeToken + "\n"
	cut := fakeToken[5:] + strings.Repeat("x", subproc.StderrTailSize-len(fakeToken[5:])-len(body)) + body
	require.Len(t, cut, subproc.StderrTailSize)

	got := stderrDetail(cut, fakeToken, mask)
	assert.False(t, strings.HasPrefix(got, fakeToken[5:]), "the cut token's end survived")
	assert.NotContains(t, got, fakeToken)
	assert.Contains(t, got, "trailing")
	assert.Contains(t, got, "next")

	// A tail below the cap is the whole stderr: its first byte stays even when
	// it matches the token's last one.
	last := fakeToken[len(fakeToken)-1:]
	assert.Equal(t, last+"o such file", stderrDetail(last+"o such file\n", fakeToken, mask))
}

func TestCancelCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Equal(t, context.Canceled, cancelCause(ctx), "no cause beyond the cancel itself")

	cause := errors.New("cost cap")
	ctx, cancelCause2 := context.WithCancelCause(t.Context())
	cancelCause2(cause)

	err := cancelCause(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cause)
}

// badSchema runs fn when the bridge reads its schema, inside Run before the
// process starts, and then fails the bridge start with a non-object schema.
type badSchema struct {
	stubTool
	fn func()
}

func (s badSchema) Schema() llm.Tool {
	s.fn()

	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: s.name, Parameters: json.RawMessage(`{"type":"string"}`)}}
}

func TestRunCanceledBeforeStart(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(fmt.Sprintf("during setup %v", during), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)

			cause := errors.New("cost cap")
			f := newFakeRun(t, fakeclaude.Scenario{Replay: fixture(t, "real/t1.jsonl")})
			f.spec.Tools = tools.NewRegistry(badSchema{stubTool{name: "echo"}, func() {
				if during {
					cancel(cause)
				}
			}})

			if !during {
				cancel(cause)
			}

			res, err := Run(ctx, f.spec)
			require.ErrorIs(t, err, cause)
			assert.Equal(t, "canceled", res.Reason)
			assert.NoFileExists(t, f.record, "the process started")
			requireNoRunDirs(t, f.spec.Home)
		})
	}
}
