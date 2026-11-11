package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"

	"github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
	"github.com/mhersson/contextmatrix-vendorkit/subproc"
)

const (
	// gateTimeout bounds the wait for claude's answer to the initialize
	// request; the task is written only after it.
	gateTimeout = 30 * time.Second
	// stopGrace is the SIGTERM-to-SIGKILL grace, and the time a process that
	// finished its work gets to exit once its stdin closes.
	stopGrace = 2 * time.Second
)

// runOpts are Run's fixed settings; tests vary them.
type runOpts struct {
	gateTimeout time.Duration
	grace       time.Duration
	extraEnv    []string     // appended after the env scrub; the smoke tests point claude at a mock API
	onLine      func([]byte) // every stdout line, unmasked; tests only
}

// Run executes one unit of work (an agent phase, an architect stage, a mob or
// review seat) as one `claude -p` process and returns a harness-shaped result.
// It returns the usage and messages it observed even when err != nil. A cancel
// that arrives after the result line keeps that result's outcome, as in
// harness.Run.
func Run(ctx context.Context, spec Spec) (harness.Result, error) {
	return run(ctx, spec, runOpts{gateTimeout: gateTimeout, grace: stopGrace})
}

func run(ctx context.Context, spec Spec, o runOpts) (res harness.Result, err error) {
	mask := newMask(spec.OAuthToken, spec.Redact)

	// failed reports a failure before the process starts; once ctx is done the
	// cancel is the outcome, whatever setup step it broke.
	failed := func(err error) (harness.Result, error) {
		if ctx.Err() != nil {
			return harness.Result{Reason: "canceled"}, maskErr(mask, fmt.Errorf("claudecode: run canceled: %w: %w", cancelCause(ctx), err))
		}

		return harness.Result{Reason: "error"}, maskErr(mask, err)
	}

	if err := spec.validate(false); err != nil {
		return failed(err)
	}

	if ctx.Err() != nil {
		return harness.Result{Reason: "canceled"}, maskErr(mask, fmt.Errorf("claudecode: run canceled: %w", cancelCause(ctx)))
	}

	task, err := userMessage(spec.Task, spec.TaskImages)
	if err != nil {
		return failed(err)
	}

	l, err := launch(ctx, spec, false, o.extraEnv)
	if err != nil {
		return failed(err)
	}

	defer func() {
		if rerr := l.release(mask); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()

	m := newMapper(mapperOpts{
		Emitter: spec.Emitter, Redact: spec.Redact, Secret: spec.OAuthToken,
		OnDelta: spec.OnDelta, OnRateLimit: spec.OnRateLimit,
		Prefix: bridgePrefix, Terminal: l.terminal,
		SystemPrompt: spec.SystemPrompt, Task: spec.Task, TaskImages: spec.TaskImages,
	})

	procErr := supervise(ctx, l.p, m, task, o)

	var storeErr error
	if spec.SessionID != "" || spec.Resume != "" {
		storeErr = storeTranscripts(spec.Home, l.rd.config, spec.OAuthToken)
	}

	res, err = m.finish(procErr)
	err = withStderr(err, res.Reason, stderrDetail(l.p.StderrTail(), spec.OAuthToken, mask))

	if storeErr != nil {
		err = errors.Join(err, maskErr(mask, storeErr))
	}

	return res, err
}

// bridgePrefix is what the bridge's tool names carry in the stream.
const bridgePrefix = "mcp__" + serverName + "__"

// launched is one started claude process and the run dir and bridge it owns.
type launched struct {
	rd       runDir
	bridge   *mcpbridge.Bridge // nil without Spec.Tools
	terminal func() (string, json.RawMessage, bool)
	p        *subproc.Proc
}

// launch prepares the run dir, the Resume transcript, the bridge and the run
// files, then starts claude. Bridged tool calls run under ctx. On error it
// releases whatever it created; the caller masks the returned error.
func launch(ctx context.Context, spec Spec, session bool, extraEnv []string) (_ *launched, err error) {
	rd, err := newRunDir(spec.Home)
	if err != nil {
		return nil, err
	}

	l := &launched{rd: rd}

	defer func() {
		if err != nil {
			err = errors.Join(err, l.release(func(s string) string { return s }))
		}
	}()

	if spec.Resume != "" {
		if err := restoreTranscript(spec.Home, spec.Resume, rd.config); err != nil {
			return nil, err
		}
	}

	var bi bridgeInfo

	if spec.Tools != nil {
		bridge, err := mcpbridge.Start(ctx, serverName, spec.Tools)
		if err != nil {
			return nil, err
		}

		l.bridge, l.terminal = bridge, bridge.Terminal
		bi = bridgeInfo{
			Server: bridge.Config(), HookURL: bridge.HookURL(),
			Tools: qualify(bridge, bridge.Tools()), Terminals: qualify(bridge, bridge.Terminals()),
		}
	}

	args, files, err := buildInvocation(spec, bi, rd.dir, session)
	if err != nil {
		return nil, err
	}

	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, fmt.Errorf("claudecode: write run file: %w", err)
		}
	}

	binary := spec.Binary
	if binary == "" {
		binary = "claude"
	}

	l.p, err = subproc.Start(subproc.Cmd{
		Path: binary, Args: args, Dir: spec.Dir,
		Env: append(childEnv(spec, rd.config), extraEnv...),
	})
	if err != nil {
		return nil, fmt.Errorf("claudecode: %w", err)
	}

	return l, nil
}

// release closes the bridge and removes the run dir, masking any error.
func (l *launched) release(mask func(string) string) error {
	var errs []error

	if l.bridge != nil {
		if err := l.bridge.Close(); err != nil {
			errs = append(errs, maskErr(mask, err))
		}
	}

	if err := os.RemoveAll(l.rd.dir); err != nil {
		errs = append(errs, maskErr(mask, fmt.Errorf("claudecode: remove run dir: %w", err)))
	}

	return errors.Join(errs...)
}

// supervise passes the initialize gate, writes the task, feeds stdout to m and
// ends the process. It returns what finish judges a run without a result by:
// the exit status, the gate failure or the cancellation.
func supervise(ctx context.Context, p *subproc.Proc, m *mapper, task []byte, o runOpts) error {
	reqID := "init-" + rand.Text()

	if _, err := p.Stdin().Write(initRequest(reqID)); err != nil {
		// The process is already gone; its exit status and stderr say why.
		return settle(ctx, p, o.grace, true)
	}

	gate := time.NewTimer(o.gateTimeout)
	defer gate.Stop()

	gateC := gate.C
	wrote := make(chan struct{})

	defer func() {
		if gateC == nil {
			<-wrote
		}
	}()

	for {
		select {
		case line, ok := <-p.Lines():
			if !ok {
				return settle(ctx, p, o.grace, true)
			}

			if o.onLine != nil {
				o.onLine(line)
			}

			if gateC != nil && isInitResponse(line, reqID) {
				gateC = nil

				// A large task can fill the pipe, so stdout keeps draining while it
				// goes in. A failed write means the process is gone; its exit
				// status says why.
				go func() {
					defer close(wrote)

					_, _ = p.Stdin().Write(task)
				}()

				continue
			}

			if m.feed(line) == stopNow {
				return settle(ctx, p, o.grace, m.sawResult)
			}
		case <-gateC:
			_ = settle(ctx, p, o.grace, false)

			return fmt.Errorf("no answer to the initialize request within %s", o.gateTimeout)
		case <-ctx.Done():
			_ = settle(ctx, p, o.grace, false)

			return cancelCause(ctx)
		}
	}
}

// settle ends the process and returns its exit error. graceful closes stdin
// and gives a process that finished its work the grace to exit on its own.
func settle(ctx context.Context, p *subproc.Proc, grace time.Duration, graceful bool) error {
	waited := make(chan error, 1)

	go func() { waited <- p.Wait() }()

	if graceful {
		_ = p.Stdin().Close()

		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case err := <-waited:
			return err
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	stopErr := p.Stop(grace)

	return errors.Join(<-waited, stopErr)
}

// cancelCause keeps the caller's cause (the architect's cost cap) matchable.
func cancelCause(ctx context.Context) error {
	// With no cause given, Cause returns ctx.Err() itself.
	if cause := context.Cause(ctx); cause != ctx.Err() {
		return fmt.Errorf("%w: %w", ctx.Err(), cause)
	}

	return ctx.Err()
}

func initRequest(id string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "control_request", "request_id": id, "request": map[string]string{"subtype": "initialize"},
	})

	return append(b, '\n')
}

func isInitResponse(line []byte, id string) bool {
	if !bytes.Contains(line, []byte(`"control_response"`)) {
		return false
	}

	var l struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
	}

	return json.Unmarshal(line, &l) == nil && l.Type == "control_response" && l.Response.RequestID == id
}

func qualify(b *mcpbridge.Bridge, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = b.QualifiedName(n)
	}

	return out
}

func maskErr(mask func(string) string, err error) error {
	return &maskedError{text: mask(err.Error()), err: err}
}

// withStderr adds the stderr tail to an untyped failure; a usage limit, a
// login failure or a cancellation already says what happened.
func withStderr(err error, reason, detail string) error {
	var ule *UsageLimitError

	if err == nil || reason != "error" || detail == "" || errors.Is(err, ErrNotLoggedIn) || errors.As(err, &ule) {
		return err
	}

	return &maskedError{text: err.Error() + "; stderr: " + detail, err: err}
}

// stderrDetail is the stderr tail as an error carries it. A tail at the cap
// was cut at a byte count, so it can open with the end of the token; that end
// goes too.
func stderrDetail(tail, secret string, mask func(string) string) string {
	for n := len(secret) - 1; n > 0 && len(tail) >= subproc.StderrTailSize; n-- {
		if strings.HasPrefix(tail, secret[len(secret)-n:]) {
			tail = tail[n:]

			break
		}
	}

	return strings.TrimSpace(mask(strings.ToValidUTF8(tail, "")))
}

// runDir is one process start's Home/.vendorkit/run-* directory and the empty
// config/ inside it that becomes CLAUDE_CONFIG_DIR.
type runDir struct{ dir, config string }

func newRunDir(home string) (runDir, error) {
	root := filepath.Join(home, ".vendorkit")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return runDir{}, fmt.Errorf("claudecode: create %s: %w", root, err)
	}

	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return runDir{}, fmt.Errorf("claudecode: create run dir: %w", err)
	}

	config := filepath.Join(dir, "config")
	if err := os.Mkdir(config, 0o700); err != nil {
		return runDir{}, errors.Join(fmt.Errorf("claudecode: create config dir: %w", err), os.RemoveAll(dir))
	}

	return runDir{dir: dir, config: config}, nil
}

// sessionStore mirrors the config dir's projects/<cwd-key>/<id>.jsonl layout
// as <key>/<id>.jsonl; Claude Code computes the key, vendorkit only globs it.
func sessionStore(home string) string {
	return filepath.Join(home, ".vendorkit", "sessions")
}

// restoreTranscript copies session id's stored transcript into a fresh config
// dir, the only state a resumed start needs.
func restoreTranscript(home, id, configDir string) error {
	matches, err := filepath.Glob(filepath.Join(sessionStore(home), "*", id+".jsonl"))
	if err != nil {
		return fmt.Errorf("claudecode: find transcript: %w", err)
	}

	restored := 0

	for _, src := range matches {
		ok, err := copyTranscript(src, filepath.Join(configDir, "projects", filepath.Base(filepath.Dir(src)), id+".jsonl"), "")
		if err != nil {
			return err
		}

		if ok {
			restored++
		}
	}

	if restored == 0 {
		return fmt.Errorf("claudecode: no stored transcript for session %s to resume", id)
	}

	return nil
}

// storeTranscripts copies the run's transcripts to the store, the token masked.
func storeTranscripts(home, configDir, secret string) error {
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("claudecode: find transcripts: %w", err)
	}

	var errs []error

	for _, src := range matches {
		dst := filepath.Join(sessionStore(home), filepath.Base(filepath.Dir(src)), filepath.Base(src))
		if _, err := copyTranscript(src, dst, secret); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// copyTranscript replaces dst with src (temp file plus rename) when src is a
// regular file, replacing secret when set. It reports whether it copied.
func copyTranscript(src, dst, secret string) (bool, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("claudecode: stat transcript: %w", err)
	}

	if !fi.Mode().IsRegular() {
		return false, nil
	}

	data, err := os.ReadFile(src) //nolint:gosec // a transcript under the run's config dir or the store
	if err != nil {
		return false, fmt.Errorf("claudecode: read transcript: %w", err)
	}

	if secret != "" {
		data = bytes.ReplaceAll(data, []byte(secret), []byte(redactedSecret))
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, fmt.Errorf("claudecode: create transcript dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".transcript-*")
	if err != nil {
		return false, fmt.Errorf("claudecode: create transcript: %w", err)
	}

	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return false, errors.Join(fmt.Errorf("claudecode: write transcript: %w", err), os.Remove(tmp.Name()))
	}

	if err := os.Rename(tmp.Name(), dst); err != nil {
		return false, errors.Join(fmt.Errorf("claudecode: store transcript: %w", err), os.Remove(tmp.Name()))
	}

	return true, nil
}
