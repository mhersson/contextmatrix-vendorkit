package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-vendorkit/internal/fakeclaude"
	"github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
	"github.com/mhersson/contextmatrix-vendorkit/subproc"
)

const (
	t7bFirst  = "Remember the number 7. Reply with: noted"
	t7bSecond = "Which number did I ask you to remember? Reply with the digit only."
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// t7bTurns splits real/t7b.jsonl at its second init into one fixture per turn.
func t7bTurns(t *testing.T) []string {
	t.Helper()

	lines := bytes.SplitAfter(readFixture(t, "real/t7b.jsonl"), []byte("\n"))

	var inits []int

	for i, line := range lines {
		if bytes.Contains(line, []byte(`"type":"system","subtype":"init"`)) {
			inits = append(inits, i)
		}
	}

	require.Len(t, inits, 2)

	dir := t.TempDir()
	paths := make([]string, 2)

	for i, part := range [][][]byte{lines[:inits[1]], lines[inits[1]:]} {
		paths[i] = filepath.Join(dir, fmt.Sprintf("turn%d.jsonl", i+1))
		require.NoError(t, os.WriteFile(paths[i], bytes.Join(part, nil), 0o600))
	}

	return paths
}

// newFakeSession is newFakeRun without the Run task.
func newFakeSession(t *testing.T, sc fakeclaude.Scenario) *fakeRun {
	t.Helper()

	f := newFakeRun(t, sc)
	f.spec.Task = ""

	return f
}

func sessionOpts() runOpts { return runOpts{gateTimeout: gateTimeout, grace: stopGrace} }

func startFake(t *testing.T, f *fakeRun, o runOpts) *Session {
	t.Helper()

	s, err := startSession(t.Context(), f.spec, o)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// boundCtx bounds a call so a hang fails the test instead of stalling it.
func boundCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// byTurn splits events at each user_input; events before the first one form
// their own group.
func byTurn(evs []events.Event) [][]events.Event {
	var out [][]events.Event

	for i, ev := range evs {
		if i == 0 || ev.Kind == events.UserInput {
			out = append(out, nil)
		}

		out[len(out)-1] = append(out[len(out)-1], ev)
	}

	return out
}

func requireTurnShape(t *testing.T, turn []events.Event) {
	t.Helper()

	require.NotEmpty(t, turn)
	assert.Equal(t, events.UserInput, turn[0].Kind, "the turn does not start with user_input")

	last := turn[len(turn)-1]
	assert.Equal(t, events.StateChange, last.Kind)
	assert.Equal(t, "awaiting_human", last.Data["state"], "the turn does not end awaiting_human")
}

func countUserInputs(l *eventLog) int {
	return bytes.Count(l.raw(), []byte(`"kind":"user_input"`))
}

func TestSessionTwoTurns(t *testing.T) {
	const initDelay = 300 * time.Millisecond

	f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t), InitDelay: initDelay})
	stream := &streamLog{}
	o := sessionOpts()
	o.onLine = stream.add

	started := time.Now()
	s := startFake(t, f, o)
	assert.GreaterOrEqual(t, time.Since(started), initDelay, "StartSession returned before claude answered initialize")
	assert.Regexp(t, uuidV4, s.ID())

	first, err := s.Send(boundCtx(t), t7bFirst, nil)
	require.NoError(t, err)
	assert.Equal(t, s.ID(), stream.last(t, "system", "init")["session_id"])

	second, err := s.Send(boundCtx(t), t7bSecond, nil)
	require.NoError(t, err)

	assert.Equal(t, "noted", first.Output)
	assert.Equal(t, "7", second.Output)

	for _, res := range []harness.Result{first, second} {
		assert.Equal(t, "done", res.Reason)
		assert.True(t, res.Completed)
		assert.Equal(t, 1, res.Turns)
	}

	assert.Equal(t, buckets{2, 4, 13949, 3819}, resultBuckets(first), "the first turn's result.usage")
	assert.Equal(t, buckets{2, 3, 14247, 3572}, resultBuckets(second), "the second turn's result.usage")
	assert.InDelta(t, 0.00090549, first.TotalCostUSD, 1e-12)
	assert.InDelta(t, 0.00085857, second.TotalCostUSD, 1e-12, "the turn's cost, not the cumulative total_cost_usd")

	assert.Equal(t, []string{"user", "assistant"}, roles(first.Messages), "Result.Messages holds this turn only")
	assert.Equal(t, t7bFirst, first.Messages[0].Content)
	assert.Equal(t, []string{"user", "assistant"}, roles(second.Messages))
	assert.Equal(t, t7bSecond, second.Messages[0].Content)

	evs := f.log.events(t)
	assert.Empty(t, stops(evs), "a stop before the process ended")

	turns := byTurn(evs)
	require.Len(t, turns, 2)

	for i, turn := range turns {
		requireTurnShape(t, turn)
		assert.Equal(t, map[string]any{"state": "awaiting_human", "turns": 1.0}, turn[len(turn)-1].Data)
		assert.EqualValues(t, len([]string{t7bFirst, t7bSecond}[i]), turn[0].Data["content_len"])
		assert.NotEmpty(t, turn[0].Data["message_id"])
	}

	assert.NotEqual(t, turns[0][0].Data["message_id"], turns[1][0].Data["message_id"])
	assert.Equal(t, resultBuckets(first), sumUsage(t, turns[0]))
	assert.Equal(t, resultBuckets(second), sumUsage(t, turns[1]))

	// The second turn streams no rate_limit_event; the first turn's windows carry over.
	for _, ev := range ofKind(turns[1], events.UsageKind) {
		assert.Contains(t, ev.Data, "subscription")
	}

	rec := f.recorded(t)
	requireInvocationInvariants(t, rec.Args)
	assert.False(t, rec.EarlyInput)
	assert.Equal(t, s.ID(), flagValue(t, rec.Args, "--session-id"))
	assert.NotContains(t, rec.Args, "--no-session-persistence")
	assert.NotContains(t, rec.Args, "--resume")
}

func TestSessionNoDroppedFirstMessage(t *testing.T) {
	const initDelay = 500 * time.Millisecond

	turns := t7bTurns(t)

	t.Run("the fake drops pre-readiness input", func(t *testing.T) {
		f := newFakeSession(t, fakeclaude.Scenario{Turns: turns[:1], InitDelay: initDelay})

		p, err := subproc.Start(subproc.Cmd{Path: f.spec.Binary, Dir: f.spec.Dir, Env: f.spec.Env})
		require.NoError(t, err)

		t.Cleanup(func() { _ = p.Stop(stopGrace) })

		early, err := userMessage("written at start", nil)
		require.NoError(t, err)

		_, err = p.Stdin().Write(append(initRequest("init-early"), early...))
		require.NoError(t, err)

		awaitLine(t, p, func(line []byte) bool {
			_, ok := initResponse(line, "init-early")

			return ok
		})

		late, err := userMessage(t7bFirst, nil)
		require.NoError(t, err)

		_, err = p.Stdin().Write(late)
		require.NoError(t, err)

		awaitLine(t, p, func(line []byte) bool { return bytes.Contains(line, []byte(`"type":"result"`)) })

		rec := f.recorded(t)
		assert.True(t, rec.EarlyInput)
		assert.JSONEq(t, string(late), rec.Stdin, "the fake answered the message written before readiness")
	})

	t.Run("the first Send still gets its answer", func(t *testing.T) {
		f := newFakeSession(t, fakeclaude.Scenario{Turns: turns[:1], InitDelay: initDelay})
		s := startFake(t, f, sessionOpts())

		res, err := s.Send(boundCtx(t), t7bFirst, nil)
		require.NoError(t, err)
		assert.Equal(t, "noted", res.Output)
		assert.False(t, f.recorded(t).EarlyInput)
	})
}

// awaitLine reads p's stdout until match accepts a line, failing after 10 s.
func awaitLine(t *testing.T, p *subproc.Proc, match func([]byte) bool) {
	t.Helper()

	timeout := time.After(10 * time.Second)

	for {
		select {
		case line, ok := <-p.Lines():
			require.True(t, ok, "stdout closed before the awaited line")

			if match(line) {
				return
			}
		case <-timeout:
			require.FailNow(t, "the awaited line never came")
		}
	}
}

func TestSessionSendsSerialize(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t), TurnDelay: 300 * time.Millisecond})
	s := startFake(t, f, sessionOpts())

	type sent struct {
		res harness.Result
		err error
	}

	send := func(text string) <-chan sent {
		out := make(chan sent, 1)

		go func() {
			res, err := s.Send(boundCtx(t), text, nil)
			out <- sent{res, err}
		}()

		return out
	}

	a := send("first")

	require.Eventually(t, func() bool { return countUserInputs(f.log) == 1 }, 5*time.Second, 5*time.Millisecond)

	b := send("second")
	ra, rb := <-a, <-b

	require.NoError(t, ra.err)
	require.NoError(t, rb.err)
	assert.Equal(t, "noted", ra.res.Output)
	assert.Equal(t, "7", rb.res.Output)
	assert.Equal(t, "first", ra.res.Messages[0].Content)
	assert.Equal(t, "second", rb.res.Messages[0].Content)

	turns := byTurn(f.log.events(t))
	require.Len(t, turns, 2, "the second message went in before the first turn's result")

	for _, turn := range turns {
		requireTurnShape(t, turn)
	}
}

func TestSessionSendAfterExit(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{
		Turns: t7bTurns(t)[:1], ExitAfterTurns: true, ExitCode: 1, Stderr: "boom " + fakeToken + "\n",
	})
	s := startFake(t, f, sessionOpts())

	res, err := s.Send(boundCtx(t), t7bFirst, nil)
	require.NoError(t, err)
	assert.Equal(t, "noted", res.Output)

	for range 2 {
		start := time.Now()
		res, err := s.Send(boundCtx(t), t7bSecond, nil)

		require.Error(t, err)
		assert.Less(t, time.Since(start), time.Second)
		assert.Equal(t, "error", res.Reason)
		assert.NotContains(t, err.Error(), fakeToken)
	}

	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)
}

func TestSessionCancelEndsSession(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "grandchild.pid")
	f := newFakeSession(t, fakeclaude.Scenario{
		Turns: []string{fixture(t, "synthetic/crash.jsonl")}, Hang: true, Grandchild: pidPath,
	})
	s := startFake(t, f, sessionOpts())

	ctx, cancel := context.WithCancelCause(boundCtx(t))
	defer cancel(nil)

	cause := errors.New("cost cap")
	canceledAt := make(chan time.Time, 1)

	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := os.Stat(pidPath); err == nil {
					canceledAt <- time.Now()

					cancel(cause)

					return
				}
			}
		}
	}()

	res, err := s.Send(ctx, "work", nil)
	returned := time.Now()

	require.ErrorIs(t, err, cause)
	assert.Equal(t, "canceled", res.Reason)
	assert.Less(t, returned.Sub(<-canceledAt), 3*time.Second)

	grandchild := readPID(t, pidPath)
	assert.Eventually(t, func() bool { return processGone(grandchild) }, 2*time.Second, 20*time.Millisecond,
		"grandchild %d outlived the session", grandchild)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)

	start := time.Now()
	_, err = s.Send(boundCtx(t), "again", nil)
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestSessionClose(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t)})

	keep := filepath.Join(f.spec.Home, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o600))

	s := startFake(t, f, sessionOpts())

	_, err := s.Send(boundCtx(t), t7bFirst, nil)
	require.NoError(t, err)

	first := s.Close()
	require.NoError(t, first)
	assert.True(t, processGone(f.recorded(t).PID), "Close returned before the process exited")

	start := time.Now()
	second := s.Close()

	assert.Equal(t, first, second, "a second Close")
	assert.Less(t, time.Since(start), 100*time.Millisecond)

	requireNoRunDirs(t, f.spec.Home)
	assert.FileExists(t, keep)

	_, err = s.Send(boundCtx(t), t7bSecond, nil)
	require.Error(t, err)
}

// sendInFlight starts a Send on a turn that never finishes and returns once
// the turn's first model call is under way.
func sendInFlight(t *testing.T) (*fakeRun, *Session, <-chan error) {
	t.Helper()

	f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{fixture(t, "synthetic/crash.jsonl")}, Hang: true})
	s := startFake(t, f, sessionOpts())
	errc := make(chan error, 1)

	go func() {
		res, err := s.Send(boundCtx(t), "work", nil)
		if err == nil || res.Reason != "canceled" {
			err = fmt.Errorf("send returned Reason %q, err %w", res.Reason, err)
		}

		errc <- err
	}()

	require.Eventually(t, func() bool {
		return bytes.Contains(f.log.raw(), []byte(`"kind":"model_request"`))
	}, 5*time.Second, 5*time.Millisecond)

	return f, s, errc
}

func TestSessionCloseDuringTurn(t *testing.T) {
	f, s, errc := sendInFlight(t)

	require.NoError(t, s.Close())
	require.ErrorIs(t, <-errc, context.Canceled)
	assert.True(t, processGone(f.recorded(t).PID))

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "done", evs[len(evs)-1].Data["stop"])
}

func TestSessionCancelWhileQueued(t *testing.T) {
	f, s, errc := sendInFlight(t)

	ctx, cancel := context.WithCancelCause(boundCtx(t))
	cause := errors.New("cost cap")
	cancel(cause)

	res, err := s.Send(ctx, "queued", nil)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, "canceled", res.Reason)

	require.ErrorIs(t, <-errc, cause, "the in-flight turn did not end with the session")
	assert.True(t, processGone(f.recorded(t).PID))
	assert.Equal(t, 1, countUserInputs(f.log), "the queued message was sent")

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "canceled", evs[len(evs)-1].Data["stop"])
}

func TestSessionStopOnceAtEnd(t *testing.T) {
	requireOneStopLast := func(t *testing.T, evs []events.Event, want map[string]any) {
		t.Helper()

		require.Len(t, stops(evs), 1)
		assert.Equal(t, want, evs[len(evs)-1].Data, "the stop is not the last event")
	}

	t.Run("close", func(t *testing.T) {
		f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t)})
		s := startFake(t, f, sessionOpts())

		for _, text := range []string{t7bFirst, t7bSecond} {
			_, err := s.Send(boundCtx(t), text, nil)
			require.NoError(t, err)
		}

		require.NoError(t, s.Close())
		require.NoError(t, s.Close())

		evs := f.log.events(t)
		requireOneStopLast(t, evs, map[string]any{"stop": "done", "turns": 2.0})

		turns := byTurn(evs[:len(evs)-1])
		require.Len(t, turns, 2)

		for _, turn := range turns {
			requireTurnShape(t, turn)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{fixture(t, "synthetic/crash.jsonl")}, Hang: true})
		s := startFake(t, f, sessionOpts())

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		res, err := s.Send(ctx, "work", nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, "canceled", res.Reason)

		require.NoError(t, s.Close())

		evs := f.log.events(t)
		require.Len(t, stops(evs), 1)
		assert.Equal(t, "canceled", evs[len(evs)-1].Data["stop"], "the stop is not the last event")
	})

	t.Run("exit", func(t *testing.T) {
		f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t)[:1], ExitAfterTurns: true, ExitCode: 1})
		s := startFake(t, f, sessionOpts())

		_, err := s.Send(boundCtx(t), t7bFirst, nil)
		require.NoError(t, err)

		_, err = s.Send(boundCtx(t), t7bSecond, nil)
		require.Error(t, err)

		require.NoError(t, s.Close())

		evs := f.log.events(t)
		require.Len(t, stops(evs), 1)
		assert.Equal(t, "error", evs[len(evs)-1].Data["stop"], "the stop is not the last event")
	})
}

func TestSessionResume(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	turns := t7bTurns(t)

	a := newFakeSession(t, fakeclaude.Scenario{Turns: turns[:1], Plant: true})
	a.spec.Home, a.spec.Dir = home, dir
	sa := startFake(t, a, sessionOpts())

	_, err := sa.Send(boundCtx(t), t7bFirst, nil)
	require.NoError(t, err)

	key := fakeclaude.ProjectKey(evalDir(t, dir))

	stored, err := os.ReadFile(filepath.Join(home, ".vendorkit", "sessions", key, sa.ID()+".jsonl"))
	require.NoError(t, err, "the turn's transcript is not in the store before Close")
	assert.Contains(t, string(stored), t7bFirst)

	require.NoError(t, sa.Close())

	b := newFakeSession(t, fakeclaude.Scenario{Turns: turns[1:]})
	b.spec.Home, b.spec.Dir, b.spec.Resume = home, dir, sa.ID()
	stream := &streamLog{}
	o := sessionOpts()
	o.onLine = stream.add
	sb := startFake(t, b, o)

	assert.Equal(t, sa.ID(), sb.ID())

	res, err := sb.Send(boundCtx(t), t7bSecond, nil)
	require.NoError(t, err)
	assert.Equal(t, "7", res.Output)
	assert.Equal(t, sb.ID(), stream.last(t, "system", "init")["session_id"])

	rec := b.recorded(t)
	assert.Equal(t, sa.ID(), flagValue(t, rec.Args, "--resume"))
	assert.NotContains(t, rec.Args, "--session-id")
	assert.Equal(t, []string{"projects/" + key + "/" + sa.ID() + ".jsonl"}, rec.ConfigFiles)
}

func TestSessionInitializeTimeout(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{NoInit: true})
	o := sessionOpts()
	o.gateTimeout = 300 * time.Millisecond

	start := time.Now()
	s, err := startSession(t.Context(), f.spec, o)

	require.Error(t, err)
	assert.Nil(t, s)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])
}

// usageLimitResult is a turn that ends in the 429 result a usage limit gives,
// with no rejected rate_limit_event before it.
const usageLimitResult = `{"type":"system","subtype":"init","session_id":"limit-fixture-session"}
{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your 5-hour limit · resets 5pm (UTC)","num_turns":1,"usage":{},"session_id":"limit-fixture-session"}
`

func TestSessionUsageLimitFromResultEndsSession(t *testing.T) {
	turn := filepath.Join(t.TempDir(), "limit.jsonl")
	require.NoError(t, os.WriteFile(turn, []byte(usageLimitResult), 0o600))

	// Like the CLI, the fake would wait for the next message after this result.
	f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{turn}})
	s := startFake(t, f, sessionOpts())

	res, err := s.Send(boundCtx(t), "work", nil)

	var ule *UsageLimitError
	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "five_hour", ule.Window)
	assert.Equal(t, "error", res.Reason)
	assert.True(t, processGone(f.recorded(t).PID), "the process outlived the usage limit")

	evs := f.log.events(t)
	require.GreaterOrEqual(t, len(evs), 2)
	assert.Equal(t, events.Kind("usage_limit"), evs[len(evs)-2].Kind)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])

	for _, ev := range ofKind(evs, events.StateChange) {
		assert.NotEqual(t, "awaiting_human", ev.Data["state"])
	}

	start := time.Now()
	_, err = s.Send(boundCtx(t), "again", nil)
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

// serverStatusFixture is real/t1.jsonl with its init reporting server in status.
func serverStatusFixture(t *testing.T, server, status string) string {
	t.Helper()

	data := rewrite(readFixture(t, "real/t1.jsonl"), `"mcp_servers":[]`, `"mcp_servers":[{"name":"`+server+`","status":"`+status+`"}]`)
	path := filepath.Join(t.TempDir(), "failed-server.jsonl")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func TestSessionConfiguredServerFailedEndsSession(t *testing.T) {
	for _, status := range []string{"failed", "needs-auth"} {
		t.Run(status, func(t *testing.T) {
			f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{serverStatusFixture(t, "extra", status)}})
			f.spec.MCPServers = []mcpbridge.ServerConfig{{Name: "extra", URL: "http://127.0.0.1:1/mcp"}}
			s := startFake(t, f, sessionOpts())

			res, err := s.Send(boundCtx(t), "work", nil)
			require.Error(t, err)

			var se *MCPServerError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, "extra", se.Server)
			assert.Equal(t, status, se.Status)
			assert.Equal(t, "error", res.Reason)
			assert.True(t, processGone(f.recorded(t).PID), "the process outlived the failed server")

			_, err = s.Send(boundCtx(t), "again", nil)
			require.Error(t, err)
		})
	}
}

func TestSessionStartCanceled(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{NoInit: true})

	ctx, cancel := context.WithCancelCause(boundCtx(t))
	defer cancel(nil)

	cause := errors.New("worker shutdown")

	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := os.Stat(f.record); err == nil {
					cancel(cause)

					return
				}
			}
		}
	}()

	start := time.Now()
	s, err := startSession(ctx, f.spec, sessionOpts())

	require.ErrorIs(t, err, cause)
	assert.Nil(t, s)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "canceled", evs[len(evs)-1].Data["stop"])
}

func TestSessionExitBeforeReady(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{ExitAtStart: true, ExitCode: 3, Stderr: "fatal: bad config " + fakeToken + "\n"})

	s, err := startSession(boundCtx(t), f.spec, sessionOpts())

	require.Error(t, err)
	assert.Nil(t, s)

	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 3, exit.ExitCode())
	assert.Contains(t, err.Error(), "fatal: bad config", "the stderr tail is missing from the error")
	assert.NotContains(t, err.Error(), fakeToken)
	assert.True(t, processGone(f.recorded(t).PID))
	requireNoRunDirs(t, f.spec.Home)

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])
}

func TestSessionFailsClosedOnForeignAccount(t *testing.T) {
	for name, acct := range foreignAccounts {
		t.Run(name, func(t *testing.T) {
			f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t), Account: json.RawMessage(acct)})

			s, err := startSession(boundCtx(t), f.spec, sessionOpts())

			require.ErrorIs(t, err, errNotSubscription)
			assert.Nil(t, s)
			assert.True(t, processGone(f.recorded(t).PID))
			requireNoRunDirs(t, f.spec.Home)

			// As any failed start: no turn, and the process's one stop.
			evs := f.log.events(t)
			assert.Empty(t, ofKind(evs, events.UserInput))
			require.Len(t, stops(evs), 1)
			assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])
		})
	}
}

func TestSessionProceedsWithoutAccount(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t), NoAccount: true})
	s := startFake(t, f, sessionOpts())

	res, err := s.Send(boundCtx(t), t7bFirst, nil)
	require.NoError(t, err)
	assert.Equal(t, "done", res.Reason)
	require.NoError(t, s.Close())
}

func TestSessionUsageLimitEndsSession(t *testing.T) {
	f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{fixture(t, "synthetic/rejected.jsonl")}, Hang: true})
	s := startFake(t, f, sessionOpts())

	start := time.Now()
	res, err := s.Send(boundCtx(t), "work", nil)

	assert.Less(t, time.Since(start), 3*time.Second)

	var ule *UsageLimitError
	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "seven_day", ule.Window)
	assert.Equal(t, "error", res.Reason)
	assert.True(t, processGone(f.recorded(t).PID))

	evs := f.log.events(t)
	require.Len(t, stops(evs), 1)
	assert.Equal(t, "error", evs[len(evs)-1].Data["stop"])
}

func TestSessionFailsClosedOnUnreadableAccount(t *testing.T) {
	for name, acct := range unreadableAccounts {
		t.Run(name, func(t *testing.T) {
			f := newFakeSession(t, fakeclaude.Scenario{Turns: t7bTurns(t), Account: json.RawMessage(acct)})

			start := time.Now()
			s, err := startSession(boundCtx(t), f.spec, sessionOpts())

			require.ErrorIs(t, err, errNotSubscription, "an unreadable account fails as an account error, not a gate timeout")
			assert.Less(t, time.Since(start), 5*time.Second)
			assert.Nil(t, s)
		})
	}
}

func TestSessionServerHaltEmissions(t *testing.T) {
	for _, tc := range []struct {
		name, server string
		typed        bool
	}{
		{"configured server", "extra", true},
		{"bridge", serverName, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSession(t, fakeclaude.Scenario{Turns: []string{serverStatusFixture(t, tc.server, "failed")}})
			f.spec.MCPServers = []mcpbridge.ServerConfig{{Name: "extra", URL: "http://127.0.0.1:1/mcp"}}

			var deltas []llm.Delta

			f.spec.OnDelta = func(d llm.Delta) { deltas = append(deltas, d) }
			s := startFake(t, f, sessionOpts())

			_, err := s.Send(boundCtx(t), "work", nil)
			require.Error(t, err)

			var se *MCPServerError
			assert.Equal(t, tc.typed, errors.As(err, &se))

			evs := f.log.events(t)

			var kinds []events.Kind

			for _, ev := range evs {
				kinds = append(kinds, ev.Kind)
			}

			t.Logf("emitted kinds: %v; deltas: %d", kinds, len(deltas)) // observed order
			assert.Empty(t, deltas, "no API call started")
			require.Len(t, ofKind(evs, events.UserInput), 1)
			require.Len(t, stops(evs), 1)

			if tc.typed {
				assert.Empty(t, ofKind(evs, events.ErrorKind))
			} else {
				assert.Len(t, ofKind(evs, events.ErrorKind), 1)
			}
		})
	}
}
