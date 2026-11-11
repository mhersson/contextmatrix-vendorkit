// Package fakeclaude stands in for the claude CLI in tests: the test binary
// re-executes itself as claude, replays recorded stream-json, and drives the
// real tool bridge and stop hook the way 2.1.322 does.
package fakeclaude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EnvVar names the Scenario JSON file; the test binary dispatches to Main when
// it is set.
const EnvVar = "VENDORKIT_FAKE_CLAUDE"

// sleepArg makes the re-executed binary a grandchild that sleeps until killed.
const sleepArg = "__fakeclaude_sleep"

// Scenario scripts one fake claude process. A user line that arrives before
// the initialize answer is dropped, as 2.1.322 once dropped a message written
// at process start.
type Scenario struct {
	Version     string // printed for --version
	Steps       []Step // run in order after reading the first stdin message
	Replay      string // fixture JSONL written to stdout after the steps
	Stderr      string // written to stderr before exiting
	ExitCode    int
	Hang        bool          // block after the replay or the last turn until killed
	Grandchild  string        // spawn a sleeping grandchild, write its PID to this path
	Record      string        // write Recorded JSON here
	HookOnError bool          // post PostToolUse even for an isError result (future-change guard)
	Plant       bool          // write settings.json and CLAUDE.md into CLAUDE_CONFIG_DIR, as a hostile Bash command would
	InitDelay   time.Duration // answer the initialize control_request after this long
	NoInit      bool          // never answer the initialize control_request

	// Turns replaces Steps and Replay for a long-lived session: one fixture
	// per user message received. A turn without a result line ends the process.
	Turns          []string
	TurnDelay      time.Duration // wait this long before replaying each turn
	ExitAfterTurns bool          // exit after the last turn instead of waiting for stdin to close
}

// Step is one bridged tool call the fake makes as if the model had asked.
type Step struct {
	Tool string // bare tool name on the "cm" server
	Args map[string]any
}

// Recorded is what the fake saw at start.
type Recorded struct {
	Args        []string
	EnvNames    []string          // names only, never values
	TokenSet    bool              // CLAUDE_CODE_OAUTH_TOKEN was non-empty
	Stdin       string            // the first stdin user line answered
	EarlyInput  bool              // a user line arrived before the initialize answer and was dropped
	ConfigDir   string            // CLAUDE_CONFIG_DIR
	ConfigFiles []string          // paths under CLAUDE_CONFIG_DIR at start, relative, sorted
	RunFiles    map[string]string // contents of the files --mcp-config, --settings and the prompt flags name, by flag
	PID         int
}

// ProjectKey is Claude Code's projects/ directory name for a working directory.
func ProjectKey(dir string) string {
	return nonAlnum.ReplaceAllString(dir, "-")
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// Main reads the scenario, acts and exits; it never returns.
func Main() {
	os.Exit(run())
}

func run() int {
	if slices.Contains(os.Args[1:], sleepArg) {
		for {
			time.Sleep(time.Hour)
		}
	}

	sc, err := load(os.Getenv(EnvVar))
	if err != nil {
		return fail(err)
	}

	if slices.Contains(os.Args[1:], "--version") {
		fmt.Println(sc.Version + " (Claude Code)")

		return 0
	}

	p := &proc{sc: sc, flags: parseFlags(os.Args[1:]), configDir: os.Getenv("CLAUDE_CONFIG_DIR")}

	return p.run()
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "fakeclaude:", err)

	return 2
}

func load(path string) (Scenario, error) {
	var sc Scenario

	data, err := os.ReadFile(path) //nolint:gosec // the test names the scenario file
	if err != nil {
		return sc, fmt.Errorf("read scenario: %w", err)
	}

	if err := json.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("decode scenario: %w", err)
	}

	return sc, nil
}

type flags struct {
	values    map[string]string
	noPersist bool
}

// fileFlags name files whose contents Recorded.RunFiles keeps.
var fileFlags = []string{"--mcp-config", "--settings", "--system-prompt-file", "--append-system-prompt-file"}

func parseFlags(args []string) flags {
	f := flags{values: map[string]string{}}

	for i, a := range args {
		switch a {
		case "--no-session-persistence":
			f.noPersist = true
		case "--mcp-config", "--settings", "--system-prompt-file", "--append-system-prompt-file",
			"--model", "--session-id", "--resume":
			if i+1 < len(args) {
				f.values[a] = args[i+1]
			}
		}
	}

	return f
}

type proc struct {
	sc        Scenario
	flags     flags
	configDir string
	sid       string
	in        <-chan inLine
}

type inLine struct {
	data []byte
	at   time.Time
}

func (p *proc) run() int {
	ctx := context.Background()

	rec := p.record()

	// Written again once stdin delivers the task, so a test whose run never
	// gets that far still learns the PID.
	if err := p.write(rec); err != nil {
		return fail(err)
	}

	if id := p.flags.values["--resume"]; id != "" {
		if matches, _ := filepath.Glob(filepath.Join(p.configDir, "projects", "*", id+".jsonl")); len(matches) == 0 {
			fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", id)

			return 1
		}
	}

	p.in = readLines(os.Stdin)

	first, early, ok := p.awaitUser()
	if !ok {
		return 0
	}

	rec.Stdin, rec.EarlyInput = string(first), early

	if err := p.write(rec); err != nil {
		return fail(err)
	}

	if len(p.sc.Turns) > 0 {
		return p.converse(ctx, first)
	}

	fixtures, err := p.loadFixtures(p.sc.Replay)
	if err != nil {
		return fail(err)
	}

	replay := fixtures[0]

	if err := p.persist(append([][]byte{first}, replay...)); err != nil {
		return fail(err)
	}

	if p.sc.Plant {
		if err := p.plant(); err != nil {
			return fail(err)
		}
	}

	if p.sc.Grandchild != "" {
		if err := spawnGrandchild(ctx, p.sc.Grandchild); err != nil {
			return fail(err)
		}
	}

	stopped, err := p.steps(ctx)
	if err != nil {
		return fail(err)
	}

	if stopped {
		p.drainStdin()

		return 0
	}

	for _, line := range replay {
		writeLine(line)
	}

	return p.exit(hasResult(replay))
}

// converse replays one Turns fixture per user message, as a long-lived
// stream-json session answers each message with its own init and result.
func (p *proc) converse(ctx context.Context, msg []byte) int {
	turns, err := p.loadFixtures(p.sc.Turns...)
	if err != nil {
		return fail(err)
	}

	if p.sc.Plant {
		if err := p.plant(); err != nil {
			return fail(err)
		}
	}

	if p.sc.Grandchild != "" {
		if err := spawnGrandchild(ctx, p.sc.Grandchild); err != nil {
			return fail(err)
		}
	}

	finished := false

	for i, lines := range turns {
		if i > 0 {
			var ok bool
			if msg, ok = p.nextUser(); !ok {
				return 0
			}
		}

		time.Sleep(p.sc.TurnDelay)

		if err := p.persist(append([][]byte{msg}, lines...)); err != nil {
			return fail(err)
		}

		for _, line := range lines {
			writeLine(line)
		}

		if finished = hasResult(lines); !finished {
			break
		}
	}

	return p.exit(finished && !p.sc.ExitAfterTurns)
}

// exit writes the scripted stderr and returns the exit code, hanging first
// when told to. Like the CLI, a finished turn waits for the next message
// until stdin closes when waitInput is set.
func (p *proc) exit(waitInput bool) int {
	_, _ = os.Stderr.WriteString(p.sc.Stderr)

	if p.sc.Hang {
		for {
			time.Sleep(time.Hour)
		}
	}

	if waitInput {
		p.drainStdin()
	}

	return p.sc.ExitCode
}

func hasResult(lines [][]byte) bool {
	return slices.ContainsFunc(lines, func(line []byte) bool { return lineType(line) == "result" })
}

func (p *proc) write(rec Recorded) error {
	if p.sc.Record == "" {
		return nil
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}

	tmp := p.sc.Record + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write record: %w", err)
	}

	if err := os.Rename(tmp, p.sc.Record); err != nil {
		return fmt.Errorf("publish record: %w", err)
	}

	return nil
}

func (p *proc) record() Recorded {
	rec := Recorded{
		Args:      os.Args[1:],
		TokenSet:  os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "",
		ConfigDir: p.configDir,
		RunFiles:  map[string]string{},
		PID:       os.Getpid(),
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		rec.EnvNames = append(rec.EnvNames, name)
	}

	slices.Sort(rec.EnvNames)

	for _, flag := range fileFlags {
		if path := p.flags.values[flag]; path != "" {
			data, _ := os.ReadFile(path) //nolint:gosec // the invocation names the file
			rec.RunFiles[flag] = string(data)
		}
	}

	if p.configDir != "" {
		_ = filepath.WalkDir(p.configDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}

			rel, _ := filepath.Rel(p.configDir, path)
			rec.ConfigFiles = append(rec.ConfigFiles, filepath.ToSlash(rel))

			return nil
		})
	}

	slices.Sort(rec.ConfigFiles)

	return rec
}

func readLines(r io.Reader) <-chan inLine {
	ch := make(chan inLine)

	go func() {
		defer close(ch)

		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 64<<20)

		for sc.Scan() {
			ch <- inLine{data: bytes.Clone(sc.Bytes()), at: time.Now()}
		}
	}()

	return ch
}

// awaitUser answers initialize requests until the first user line arrives
// after the answer; early reports that an earlier one was dropped.
func (p *proc) awaitUser() (line []byte, early, ok bool) {
	var answeredAt time.Time

	for l := range p.in {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}

		if json.Unmarshal(l.data, &msg) != nil {
			continue
		}

		switch {
		case msg.Type == "control_request" && msg.Request.Subtype == "initialize":
			if p.sc.NoInit {
				continue
			}

			time.Sleep(p.sc.InitDelay)

			answeredAt = time.Now()

			writeJSONLine(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": msg.RequestID,
				"response": map[string]any{"pid": os.Getpid(), "claude_code_version": p.sc.Version},
			}})
		case msg.Type == "user":
			if answeredAt.IsZero() || l.at.Before(answeredAt) {
				early = true

				continue
			}

			return l.data, early, true
		}
	}

	return nil, early, false
}

// nextUser returns the next user line; ok is false once stdin closes.
func (p *proc) nextUser() ([]byte, bool) {
	for l := range p.in {
		if lineType(l.data) == "user" {
			return l.data, true
		}
	}

	return nil, false
}

func (p *proc) drainStdin() {
	for range p.in { //nolint:revive // runs until the caller closes stdin
	}
}

// loadFixtures returns each fixture's lines (none for an empty path) with the
// fixtures' session ID replaced by this process's.
func (p *proc) loadFixtures(paths ...string) ([][][]byte, error) {
	fixtures := make([][][]byte, len(paths))

	for i, path := range paths {
		if path == "" {
			continue
		}

		data, err := os.ReadFile(path) //nolint:gosec // the test names the fixture
		if err != nil {
			return nil, fmt.Errorf("read fixture: %w", err)
		}

		for line := range bytes.Lines(data) {
			if line = bytes.TrimSpace(line); len(line) > 0 {
				fixtures[i] = append(fixtures[i], line)
			}
		}
	}

	fixtureSID := ""

	for _, line := range slices.Concat(fixtures...) {
		var l struct {
			SessionID string `json:"session_id"`
		}

		if json.Unmarshal(line, &l) == nil && l.SessionID != "" {
			fixtureSID = l.SessionID

			break
		}
	}

	p.sid = firstSet(p.flags.values["--session-id"], p.flags.values["--resume"], fixtureSID, "fake-session")

	if fixtureSID != "" && fixtureSID != p.sid {
		for _, lines := range fixtures {
			for i, line := range lines {
				lines[i] = bytes.ReplaceAll(line, []byte(fixtureSID), []byte(p.sid))
			}
		}
	}

	return fixtures, nil
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return ""
}

// persist appends lines to the session transcript, as the CLI does unless
// told not to persist.
func (p *proc) persist(lines [][]byte) error {
	if p.flags.noPersist || p.configDir == "" {
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	dir := filepath.Join(p.configDir, "projects", ProjectKey(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}

	path := filepath.Join(dir, p.sid+".jsonl")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // under CLAUDE_CONFIG_DIR
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}

	for _, line := range lines {
		if _, err := f.Write(append(bytes.Clone(line), '\n')); err != nil {
			_ = f.Close()

			return fmt.Errorf("write transcript: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close transcript: %w", err)
	}

	return nil
}

func (p *proc) plant() error {
	for name, data := range map[string]string{
		"settings.json": `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo planted"}]}]}}`,
		"CLAUDE.md":     "Planted instructions.\n",
	} {
		if err := os.WriteFile(filepath.Join(p.configDir, name), []byte(data), 0o600); err != nil {
			return fmt.Errorf("plant %s: %w", name, err)
		}
	}

	return nil
}

// spawnGrandchild starts a sleeper in this process's group and publishes its
// PID atomically.
func spawnGrandchild(ctx context.Context, pidPath string) error {
	cmd := exec.CommandContext(ctx, os.Args[0], sleepArg) //nolint:gosec // re-executes this binary
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start grandchild: %w", err)
	}

	tmp := pidPath + ".tmp"
	if err := os.WriteFile(tmp, fmt.Appendf(nil, "%d", cmd.Process.Pid), 0o600); err != nil { //nolint:gosec // the test names the PID file
		return fmt.Errorf("write grandchild pid: %w", err)
	}

	if err := os.Rename(tmp, pidPath); err != nil {
		return fmt.Errorf("publish grandchild pid: %w", err)
	}

	return nil
}

type mcpConfig struct {
	MCPServers map[string]struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"mcpServers"`
}

type settings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// steps makes each scripted call through the bridge and reports whether a stop
// hook ended the run.
func (p *proc) steps(ctx context.Context) (bool, error) {
	if len(p.sc.Steps) == 0 {
		return false, nil
	}

	var mc mcpConfig
	if err := readJSON(p.flags.values["--mcp-config"], &mc); err != nil {
		return false, err
	}

	var st settings
	if err := readJSON(p.flags.values["--settings"], &st); err != nil {
		return false, err
	}

	srv, ok := mc.MCPServers["cm"]
	if !ok {
		return false, errors.New("mcp config has no cm server")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "fakeclaude", Version: "1"}, nil)

	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: headerRT{srv.Headers}},
	}, nil)
	if err != nil {
		return false, fmt.Errorf("connect bridge: %w", err)
	}

	defer func() { _ = sess.Close() }()

	model := p.flags.values["--model"]

	for i, step := range p.sc.Steps {
		name := "mcp__cm__" + step.Tool
		id := fmt.Sprintf("toolu_fake_%d", i+1)

		args := step.Args
		if args == nil {
			args = map[string]any{}
		}

		writeJSONLine(map[string]any{"type": "assistant", "session_id": p.sid, "message": map[string]any{
			"id": fmt.Sprintf("msg_fake_%d", i+1), "type": "message", "role": "assistant", "model": model,
			"content":     []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": args}},
			"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}})

		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: step.Tool, Arguments: args})
		if err != nil {
			return false, fmt.Errorf("call %s: %w", step.Tool, err)
		}

		var text []string

		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text = append(text, tc.Text)
			}
		}

		writeJSONLine(map[string]any{"type": "user", "session_id": p.sid, "message": map[string]any{
			"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": strings.Join(text, "\n"), "is_error": res.IsError,
			}},
		}})

		// 2.1.322 fires PostToolUseFailure, never PostToolUse, for an isError result.
		event := "PostToolUse"
		if res.IsError && !p.sc.HookOnError {
			event = "PostToolUseFailure"
		}

		stop, err := p.postHooks(ctx, st, event, name, args)
		if err != nil {
			return false, err
		}

		if stop {
			n := i + 1
			writeJSONLine(map[string]any{
				"type": "result", "subtype": "success", "is_error": false, "num_turns": n, "result": "",
				"stop_reason": "tool_use", "terminal_reason": "hook_stopped", "session_id": p.sid,
				"usage": map[string]any{
					"input_tokens": n, "output_tokens": n, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
				},
			})

			return true, nil
		}
	}

	return false, nil
}

// postHooks POSTs event to every http hook whose matcher matches tool and
// reports whether one answered continue:false.
func (p *proc) postHooks(ctx context.Context, st settings, event, tool string, input map[string]any) (bool, error) {
	stop := false

	for _, m := range st.Hooks[event] {
		if m.Matcher != "" && m.Matcher != "*" {
			if ok, err := regexp.MatchString(m.Matcher, tool); err != nil || !ok {
				continue
			}
		}

		for _, h := range m.Hooks {
			if h.Type != "http" {
				continue
			}

			body, _ := json.Marshal(map[string]any{
				"session_id": p.sid, "hook_event_name": event, "tool_name": tool, "tool_input": input,
			})

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
			if err != nil {
				return false, fmt.Errorf("hook request: %w", err)
			}

			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false, fmt.Errorf("post hook: %w", err)
			}

			var out struct {
				Continue *bool `json:"continue"`
			}

			_ = json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()

			stop = stop || (out.Continue != nil && !*out.Continue)
		}
	}

	return stop, nil
}

type headerRT struct{ headers map[string]string }

func (h headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}

	return http.DefaultTransport.RoundTrip(req)
}

func lineType(line []byte) string {
	var l struct {
		Type string `json:"type"`
	}

	_ = json.Unmarshal(line, &l)

	return l.Type
}

func writeLine(line []byte) {
	_, _ = os.Stdout.Write(append(bytes.Clone(line), '\n'))
}

func writeJSONLine(v any) {
	b, _ := json.Marshal(v)
	writeLine(b)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // the invocation names the file
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}
