package claudecode

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// errSessionClosed finishes a turn that Close cut short as canceled.
var errSessionClosed = fmt.Errorf("claudecode: session closed: %w", context.Canceled)

// Session is one long-lived `claude -p --input-format stream-json` process
// that answers a conversation turn by turn (chat). It owns the process group:
// Close, a cancelled Send or the process's own exit ends it, and the session
// then emits its single state_change stop.
type Session struct {
	spec  Spec
	o     runOpts
	mask  func(string) string
	id    string
	reqID string
	l     *launched

	sem chan struct{} // held for a whole Send

	// The last rate-limit windows, carried into the next turn; guarded by sem.
	subscription []protocol.SubscriptionUsage

	ready chan struct{} // closed at the initialize answer
	ended chan struct{} // closed once the end path has run

	mu        sync.Mutex
	turn      *turn // the in-flight turn, fed by the reader
	exited    bool
	endReason string // the stop an end requested by vendorkit emits
	endCause  error  // what an in-flight turn is finished with

	// Written by the reader goroutine; read by others after ended closes.
	startErr   error // the initialize answer's account failed checkAccount
	turns      int
	stopReason string
	exitErr    error
	stderr     string
	endErr     error

	closeOnce sync.Once
	closeErr  error
}

// turn is one Send's mapper and outcome; done closes once res and err are set.
type turn struct {
	m    *mapper
	done chan struct{}
	res  harness.Result
	err  error
}

// StartSession spawns a long-lived `claude -p --input-format stream-json
// --output-format stream-json` process and returns once it is ready to accept
// the first message: claude has answered the initialize request, since a
// message written before that can be dropped. ctx bounds only the start; the
// start fails when ctx ends or no answer comes within 30 s. A non-empty Task
// fails validation (send it with Send); TaskImages and MaxBudgetUSD are
// ignored (Send takes the images). MaxTurns applies per turn. The session is
// always persisted under Home: SessionID and Resume work as in Run, and with
// neither it gets a fresh ID.
func StartSession(ctx context.Context, spec Spec) (*Session, error) {
	return startSession(ctx, spec, runOpts{gateTimeout: gateTimeout, grace: stopGrace})
}

func startSession(ctx context.Context, spec Spec, o runOpts) (*Session, error) {
	mask := newMask(spec.OAuthToken, spec.Redact)

	canceled := func(err error) error {
		if err == nil {
			return maskErr(mask, fmt.Errorf("claudecode: session start canceled: %w", cancelCause(ctx)))
		}

		return maskErr(mask, fmt.Errorf("claudecode: session start canceled: %w: %w", cancelCause(ctx), err))
	}

	if err := spec.validate(true); err != nil {
		return nil, maskErr(mask, err)
	}

	if ctx.Err() != nil {
		return nil, canceled(nil)
	}

	id := spec.SessionID + spec.Resume // validate allows at most one
	if id == "" {
		id = newSessionID()
		spec.SessionID = id
	}

	// The session outlives ctx, so bridged tool calls must too.
	l, err := launch(context.WithoutCancel(ctx), spec, true, o.extraEnv)
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(err)
		}

		return nil, maskErr(mask, err)
	}

	s := &Session{
		spec: spec, o: o, mask: mask, id: id, reqID: "init-" + rand.Text(), l: l,
		sem:   make(chan struct{}, 1),
		ready: make(chan struct{}),
		ended: make(chan struct{}),
	}

	go s.read()

	// A failed write means the process is gone; the reader sees it exit.
	_, _ = l.p.Stdin().Write(initRequest(s.reqID))

	gate := time.NewTimer(o.gateTimeout)
	defer gate.Stop()

	var startErr error

	select {
	case <-s.ready:
		return s, nil
	case <-s.ended:
		startErr = errors.New("claudecode: claude exited before answering the initialize request")

		switch {
		case s.startErr != nil:
			startErr = s.startErr
		case s.exitErr != nil:
			startErr = fmt.Errorf("%w: %w", startErr, s.exitErr)
		}
	case <-gate.C:
		startErr = fmt.Errorf("claudecode: no answer to the initialize request within %s", o.gateTimeout)
		_ = s.requestEnd("error", nil)
	case <-ctx.Done():
		_ = s.requestEnd("canceled", nil)
		<-s.ended

		return nil, errors.Join(canceled(nil), s.endErr)
	}

	<-s.ended

	return nil, errors.Join(withStderr(maskErr(mask, startErr), "error", s.stderr), s.endErr)
}

// Send writes one user message and blocks until that turn's result. Sends
// serialize: a Send during a turn waits for it. Cancelling ctx, even while
// waiting for its turn, ends the session (as Close) and returns Reason
// "canceled" with an error wrapping context.Cause(ctx); a result that came
// first keeps its outcome. Send emits user_input first and state_change
// awaiting_human after the turn, and stores the transcript before it returns.
// Result.Messages holds this turn only. A usage limit ends the session. Send on
// a session whose process has exited returns an error at once. Never call it
// from Spec.OnDelta, Spec.OnRateLimit or the Emitter's writers: the session
// calls them mid-turn or while ending, so the call deadlocks.
func (s *Session) Send(ctx context.Context, text string, images []llm.ImageURL) (harness.Result, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return s.cancel(ctx)
	}

	defer func() { <-s.sem }()

	if ctx.Err() != nil {
		return s.cancel(ctx)
	}

	msg, err := userMessage(text, images)
	if err != nil {
		return harness.Result{Reason: "error"}, maskErr(s.mask, err)
	}

	m := newMapper(mapperOpts{
		Emitter: s.spec.Emitter, Redact: s.spec.Redact, Secret: s.spec.OAuthToken,
		OnDelta: s.spec.OnDelta, OnRateLimit: s.spec.OnRateLimit,
		Prefix: bridgePrefix, Servers: serverNames(s.spec.MCPServers), Session: true, Task: text, TaskImages: images,
	})
	m.subscription = s.subscription

	t := &turn{m: m, done: make(chan struct{})}

	// user_input goes out under mu, so it cannot follow the end path's stop.
	s.mu.Lock()

	if s.exited {
		s.mu.Unlock()
		<-s.ended

		return harness.Result{Reason: "error"}, s.endedError()
	}

	s.turn = t
	s.spec.Emitter.Emit(events.UserInput, map[string]any{"message_id": "msg-" + rand.Text(), "content_len": len(text)})
	s.mu.Unlock()

	wrote := make(chan struct{})

	go func() {
		defer close(wrote)

		// A failed write means the process is gone; the reader ends the turn.
		_, _ = s.l.p.Stdin().Write(msg)
	}()

	select {
	case <-t.done:
	case <-ctx.Done():
		select {
		case <-t.done:
		default:
			_ = s.requestEnd("canceled", cancelCause(ctx))

			<-t.done
		}
	}

	// The end path closes stdin before it closes done, so this cannot block.
	<-wrote

	s.subscription = m.subscription

	return t.res, t.err
}

// Close ends the session: SIGTERM to the process group, SIGKILL after a grace
// period, then waits for the exit and the cleanup. A turn in flight returns
// Reason "canceled". A second Close returns the first one's result. Never call
// it from Spec.OnDelta, Spec.OnRateLimit or the Emitter's writers (see Send).
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		var stopErr error
		if err := s.requestEnd("done", errSessionClosed); err != nil {
			stopErr = maskErr(s.mask, err)
		}

		<-s.ended

		s.closeErr = errors.Join(stopErr, s.endErr)
	})

	return s.closeErr
}

// ID is the session ID, known when StartSession returns: SessionID, Resume
// (2.1.322 keeps the ID on --resume) or a UUID passed as --session-id
// (2.1.322 emits no init before the first message). Callers store it after
// every StartSession, a resumed one included.
func (s *Session) ID() string { return s.id }

// cancel ends the session for a Send whose ctx ended before its turn began.
func (s *Session) cancel(ctx context.Context) (harness.Result, error) {
	cause := cancelCause(ctx)

	_ = s.requestEnd("canceled", cause)
	<-s.ended

	return harness.Result{Reason: "canceled"}, maskErr(s.mask, fmt.Errorf("claudecode: session canceled: %w", cause))
}

// requestEnd stops the process; the first request names the stop and the
// cause an in-flight turn is finished with. The reader runs the end path.
func (s *Session) requestEnd(reason string, cause error) error {
	s.mu.Lock()

	if s.endReason == "" {
		s.endReason, s.endCause = reason, cause
	}

	s.mu.Unlock()

	return s.l.p.Stop(s.o.grace)
}

func (s *Session) endedError() error {
	return withStderr(maskErr(s.mask, fmt.Errorf("claudecode: session ended (%s)", s.stopReason)), s.stopReason, s.stderr)
}

// read is the session's only stdout reader. It passes the readiness gate,
// feeds the in-flight turn's mapper and, once stdout closes and the process is
// reaped, runs the end path.
func (s *Session) read() {
	ready, halted := false, false

	for line := range s.l.p.Lines() {
		if s.o.onLine != nil {
			s.o.onLine(line)
		}

		if !ready {
			if acct, ok := initResponse(line, s.reqID); ok {
				if err := s.o.checkAccount(acct); err != nil {
					s.startErr = err
					_ = s.requestEnd("error", nil)

					continue
				}

				ready = true

				close(s.ready)
			}

			continue
		}

		s.mu.Lock()
		t := s.turn
		s.mu.Unlock()

		if t == nil || halted {
			continue
		}

		v := t.m.feed(line)

		switch {
		case v == stopNow || t.m.limit != nil:
			// A usage limit, from a rate_limit_event or the result, or an MCP
			// server reporting failed or needs-auth stops the process; the end
			// path finishes the turn.
			halted = true

			_ = s.requestEnd("error", nil)
		case t.m.sawResult:
			s.mu.Lock()
			s.turn = nil
			s.mu.Unlock()

			s.completeTurn(t)
		}
	}

	s.end(ready, s.l.p.Wait())
}

func (s *Session) completeTurn(t *turn) {
	res, err := t.m.finish(nil)

	if serr := storeTranscripts(s.spec.Home, s.l.rd.config, s.spec.OAuthToken); serr != nil {
		err = errors.Join(err, maskErr(s.mask, serr))
	}

	s.turns += res.Turns
	t.res, t.err = res, err

	close(t.done)
}

// end runs once, on the reader goroutine, after the process is reaped: it
// finishes an in-flight turn, stores the transcript, emits the session's one
// stop, closes the bridge and removes the run dir.
func (s *Session) end(ready bool, procErr error) {
	s.mu.Lock()
	s.exited = true
	t, reason, cause := s.turn, s.endReason, s.endCause
	s.turn = nil
	s.mu.Unlock()

	// Unblocks a Send still writing to a process that is gone.
	_ = s.l.p.Stdin().Close()

	s.stderr = stderrDetail(s.l.p.StderrTail(), s.spec.OAuthToken, s.mask)

	if t != nil {
		turnErr := procErr
		if cause != nil {
			turnErr = cause
		}

		res, err := t.m.finish(turnErr)
		t.res, t.err = res, withStderr(err, res.Reason, s.stderr)
		s.turns += res.Turns
	}

	if reason == "" {
		reason = "error"
		if ready && procErr == nil && t == nil {
			reason = "done"
		}
	}

	var errs []error

	if err := storeTranscripts(s.spec.Home, s.l.rd.config, s.spec.OAuthToken); err != nil {
		errs = append(errs, maskErr(s.mask, err))
	}

	s.spec.Emitter.Emit(events.StateChange, map[string]any{"stop": reason, "turns": s.turns})

	if err := s.l.release(s.mask); err != nil {
		errs = append(errs, err)
	}

	s.stopReason, s.exitErr, s.endErr = reason, procErr, errors.Join(errs...)

	if t != nil {
		close(t.done)
	}

	close(s.ended)
}

// newSessionID returns a random UUID v4, the form --session-id takes.
func newSessionID() string {
	var b [16]byte

	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
