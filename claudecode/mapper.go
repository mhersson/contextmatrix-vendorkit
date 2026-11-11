package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

type mapperOpts struct {
	Emitter     *events.Emitter
	Redact      func(string) string // nil = identity
	Secret      string              // always masked in every emitted or returned string
	OnDelta     func(llm.Delta)
	OnRateLimit func(RateLimit)
	Prefix      string // "mcp__cm__", stripped from bridged tool names
	Terminal    func() (string, json.RawMessage, bool)
	Session     bool // per-turn accounting from model_usage_since_last_result

	SystemPrompt string // seeds a system message in Result.Messages
	Task         string
	TaskImages   []llm.ImageURL
}

type verdict int

const (
	keepGoing verdict = iota
	stopNow           // the caller stops the process; finish explains why
)

// syntheticModel marks a message Claude Code made up locally (an auth or API
// error), not an API call.
const syntheticModel = "<synthetic>"

type tokenBuckets struct{ prompt, completion, cacheRead, cacheCreation int64 }

func (b tokenBuckets) add(o tokenBuckets) tokenBuckets {
	return tokenBuckets{b.prompt + o.prompt, b.completion + o.completion, b.cacheRead + o.cacheRead, b.cacheCreation + o.cacheCreation}
}

// remainder is what b holds beyond streamed, clamped at 0 per bucket.
func (b tokenBuckets) remainder(streamed tokenBuckets) tokenBuckets {
	return tokenBuckets{
		max(b.prompt-streamed.prompt, 0), max(b.completion-streamed.completion, 0),
		max(b.cacheRead-streamed.cacheRead, 0), max(b.cacheCreation-streamed.cacheCreation, 0),
	}
}

type toolUse struct {
	id, name  string
	input     json.RawMessage // from the assistant line
	fragments strings.Builder // input_json_delta, used when no assistant line carried the input
}

func (t *toolUse) args() string {
	switch {
	case len(t.input) > 0:
		return string(t.input)
	case t.fragments.Len() > 0:
		return t.fragments.String()
	default:
		return "{}"
	}
}

// apiCall is the open call: one message.id.
type apiCall struct {
	id, model   string
	turn        int
	text        []string
	thinking    []string
	tools       []*toolUse
	toolOfBlock map[int]int // stream content-block index to tools ordinal
	stopReason  string
	lineStop    string
	deltaUsage  apiUsage
	startUsage  apiUsage
	lineUsage   *apiUsage // first assistant line
}

func (c *apiCall) toolUse(id, name string) int {
	for i, t := range c.tools {
		if t.id == id {
			return i
		}
	}

	c.tools = append(c.tools, &toolUse{id: id, name: name})

	return len(c.tools) - 1
}

// usage takes each bucket from message_delta, else message_start, else the
// first assistant line.
func (c *apiCall) usage() tokenBuckets {
	var line apiUsage
	if c.lineUsage != nil {
		line = *c.lineUsage
	}

	pick := func(f func(apiUsage) *int64) int64 {
		for _, u := range []apiUsage{c.deltaUsage, c.startUsage, line} {
			if v := f(u); v != nil {
				return *v
			}
		}

		return 0
	}

	return tokenBuckets{
		prompt:        pick(func(u apiUsage) *int64 { return u.InputTokens }),
		completion:    pick(func(u apiUsage) *int64 { return u.OutputTokens }),
		cacheRead:     pick(func(u apiUsage) *int64 { return u.CacheReadInputTokens }),
		cacheCreation: pick(func(u apiUsage) *int64 { return u.CacheCreationInputTokens }),
	}
}

// overlay copies the buckets src reports into dst.
func overlay(dst *apiUsage, src apiUsage) {
	for _, p := range []struct{ d, s **int64 }{
		{&dst.InputTokens, &src.InputTokens},
		{&dst.OutputTokens, &src.OutputTokens},
		{&dst.CacheReadInputTokens, &src.CacheReadInputTokens},
		{&dst.CacheCreationInputTokens, &src.CacheCreationInputTokens},
	} {
		if *p.s != nil {
			*p.d = *p.s
		}
	}
}

// mapper turns stream-json lines into harness events and a harness.Result.
type mapper struct {
	o          mapperOpts
	mask       func(string) string
	maskSecret func(string) string

	open   *apiCall
	closed map[string]bool
	calls  int

	streamed      map[string]tokenBuckets // per model
	streamedTotal tokenBuckets
	subscription  []protocol.SubscriptionUsage
	lastText      string

	sawResult bool
	finished  bool

	halted     bool // a stop condition was seen; later lines are ignored
	backstop   bool
	limit      *UsageLimitError
	authFailed bool
	bridgeErr  error
	lastRL     RateLimit

	sid     string
	version string // claude_code_version from init
	res     harness.Result
	err     error

	messages []llm.Message
}

func newMapper(o mapperOpts) *mapper {
	m := &mapper{
		o:          o,
		mask:       newMask(o.Secret, o.Redact),
		maskSecret: newMask(o.Secret, nil),
		closed:     map[string]bool{},
		streamed:   map[string]tokenBuckets{},
	}
	m.messages = m.seedMessages()

	return m
}

// feed consumes one stream-json line. Lines the mapper does not know, or that
// are not JSON, are ignored.
func (m *mapper) feed(line []byte) verdict {
	if m.halted {
		return stopNow
	}

	if m.sawResult || m.finished {
		return m.afterResult()
	}

	l, ok := decodeLine(line)
	if !ok {
		return keepGoing
	}

	switch l.Type {
	case "assistant":
		if l.Error == "authentication_failed" {
			m.authFailed = true
		}

		m.onAssistant(l.Message)
	case "user":
		if !l.IsReplay {
			m.onUser(l.Message)
		}
	case "stream_event":
		m.onStreamEvent(l.Event, l.APIMessageID)
	case "rate_limit_event":
		m.onRateLimit(l.RateLimitInfo)
	case "system":
		switch l.Subtype {
		case "init":
			m.onInit(l)
		case "compact_boundary":
			m.emit(events.StateChange, map[string]any{"event": "compaction"})
		}
	case "result":
		m.onResult(l)

		return m.afterResult()
	}

	if m.halted {
		return stopNow
	}

	return keepGoing
}

// sessionID is the session_id init reported; "" before init.
func (m *mapper) sessionID() string { return m.sid }

func (m *mapper) onInit(l streamLine) {
	m.sid = l.SessionID
	m.version = l.ClaudeCodeVersion

	// "mcp__cm__" names the server "cm".
	server := strings.TrimSuffix(strings.TrimPrefix(m.o.Prefix, "mcp__"), "__")
	if server == "" {
		return
	}

	for _, s := range l.MCPServers {
		if s.Name == server && s.Status == "failed" {
			m.bridgeErr = errors.New(m.mask(fmt.Sprintf("claudecode: MCP bridge server %q failed to start", server)))
			m.halted = true
		}
	}
}

// afterResult: a Run's unit of work is over at its result; a Session turn's
// process lives on.
func (m *mapper) afterResult() verdict {
	if m.o.Session {
		return keepGoing
	}

	return stopNow
}

// finish closes the stream. procErr is the process's exit or stop error, nil
// when it exited cleanly; one wrapping context.Canceled or DeadlineExceeded
// marks the run canceled. A result line, when one arrived, decides the outcome.
func (m *mapper) finish(procErr error) (harness.Result, error) {
	if m.finished {
		return m.res, m.err
	}

	m.finished = true

	if m.sawResult {
		return m.res, m.err
	}

	m.closeCall()

	m.res.Turns = m.calls
	m.res.PromptTokens = m.streamedTotal.prompt
	m.res.CompletionTokens = m.streamedTotal.completion
	m.res.CacheReadTokens = m.streamedTotal.cacheRead
	m.res.CacheCreationTokens = m.streamedTotal.cacheCreation
	m.res.Output = m.mask(m.lastText)

	switch {
	case m.limit != nil:
		m.conclude("error", m.limit)
	case m.bridgeErr != nil:
		m.conclude("error", m.bridgeErr)
	case m.backstop:
		// vendorkit stopped the process itself, so procErr says nothing.
		m.conclude("done", nil)
	case m.authFailed:
		m.conclude("error", m.notLoggedIn(""))
	case errors.Is(procErr, context.Canceled) || errors.Is(procErr, context.DeadlineExceeded):
		m.conclude("canceled", m.wrap("claudecode: run canceled", procErr))
	case procErr != nil:
		m.conclude("error", m.wrap("claudecode: stream ended without a result", procErr))
	default:
		m.conclude("error", errors.New("claudecode: stream ended without a result"))
	}

	return m.res, m.err
}

// notLoggedIn keeps ErrNotLoggedIn matchable with errors.Is.
func (m *mapper) notLoggedIn(detail string) error {
	text := ErrNotLoggedIn.Error()
	if detail != "" {
		text += ": " + detail
	}

	return &maskedError{text: m.mask(text), err: ErrNotLoggedIn}
}

func (m *mapper) wrap(msg string, err error) error {
	return &maskedError{text: m.mask(msg + ": " + err.Error()), err: err}
}

// emit masks the secret in every string value, so ids and names are covered
// too; content, args, thinking and errors also pass Redact at their call sites.
func (m *mapper) emit(kind events.Kind, data map[string]any) {
	if m.o.Emitter == nil {
		return
	}

	for k, v := range data {
		if s, ok := v.(string); ok {
			data[k] = m.maskSecret(s)
		}
	}

	m.o.Emitter.Emit(kind, data)
}

func (m *mapper) delta(d llm.Delta) {
	if m.o.OnDelta == nil {
		return
	}

	d.Content = m.maskSecret(d.Content)
	d.Reasoning = m.maskSecret(d.Reasoning)

	if d.ToolCall != nil {
		tc := *d.ToolCall
		tc.ID = m.maskSecret(tc.ID)
		tc.Name = m.maskSecret(tc.Name)
		tc.ArgsFragment = m.maskSecret(tc.ArgsFragment)
		d.ToolCall = &tc
	}

	m.o.OnDelta(d)
}

// toolName drops only the bridge prefix, so other servers' tools keep their
// full mcp__<server>__<tool> name.
func (m *mapper) toolName(name string) string {
	if m.o.Prefix != "" {
		name = strings.TrimPrefix(name, m.o.Prefix)
	}

	return name
}

// callFor returns the open call for id, closing a different open call and
// opening a new one as needed; nil for a call that already closed.
func (m *mapper) callFor(id, model string) *apiCall {
	if m.open != nil && m.open.id == id {
		return m.open
	}

	if m.closed[id] {
		return nil
	}

	m.closeCall()

	// After an accepted terminal call, a new API call means the model ignored
	// the end of the run.
	if !m.o.Session && m.o.Terminal != nil {
		if _, _, ok := m.o.Terminal(); ok {
			m.backstop = true
			m.halted = true

			return nil
		}
	}

	m.calls++
	m.open = &apiCall{id: id, model: model, turn: m.calls, toolOfBlock: map[int]int{}}

	if model != "" {
		m.res.ModelUsed = model
	}

	m.emit(events.ModelRequest, map[string]any{"turn": m.calls, "model": model})

	return m.open
}

func (m *mapper) onAssistant(msg *apiMessage) {
	if msg == nil || msg.ID == "" || msg.Model == syntheticModel {
		return
	}

	c := m.callFor(msg.ID, msg.Model)
	if c == nil {
		return
	}

	if c.model == "" && msg.Model != "" {
		c.model = msg.Model
		m.res.ModelUsed = msg.Model
	}

	if c.lineUsage == nil {
		u := msg.Usage
		c.lineUsage = &u
	}

	if msg.StopReason != "" {
		c.lineStop = msg.StopReason
	}

	for _, b := range blocks(msg.Content) {
		switch b.Type {
		case "text":
			c.text = append(c.text, b.Text)
		case "thinking":
			if b.Thinking != "" {
				c.thinking = append(c.thinking, b.Thinking)
			}
		case "tool_use":
			c.tools[c.toolUse(b.ID, b.Name)].input = b.Input
		}
	}
}

func (m *mapper) onStreamEvent(ev *streamEvent, apiMessageID string) {
	if ev == nil {
		return
	}

	if ev.Type == "message_start" {
		if ev.Message == nil || ev.Message.ID == "" {
			return
		}

		if c := m.callFor(ev.Message.ID, ev.Message.Model); c != nil {
			c.startUsage = ev.Message.Usage
		}

		return
	}

	c := m.streamCall(apiMessageID)
	if c == nil {
		return
	}

	switch ev.Type {
	case "content_block_start":
		if b := ev.ContentBlock; b != nil && b.Type == "tool_use" {
			i := c.toolUse(b.ID, b.Name)
			c.toolOfBlock[ev.Index] = i
			m.delta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: i, ID: b.ID, Name: m.toolName(b.Name)}})
		}
	case "content_block_delta":
		m.onBlockDelta(c, ev)
	case "message_delta":
		if ev.Usage != nil {
			overlay(&c.deltaUsage, *ev.Usage)
		}

		if ev.Delta != nil && ev.Delta.StopReason != "" {
			c.stopReason = ev.Delta.StopReason
		}
	case "message_stop":
		m.closeCall()
	}
}

// streamCall returns the open call a stream event belongs to. An event of
// another, unseen call closes the open one.
func (m *mapper) streamCall(apiMessageID string) *apiCall {
	if m.open == nil {
		return nil
	}

	if apiMessageID == "" || apiMessageID == m.open.id {
		return m.open
	}

	if !m.closed[apiMessageID] {
		m.closeCall()
	}

	return nil
}

func (m *mapper) onBlockDelta(c *apiCall, ev *streamEvent) {
	d := ev.Delta
	if d == nil {
		return
	}

	switch d.Type {
	case "text_delta":
		m.delta(llm.Delta{Content: d.Text})
	case "thinking_delta":
		m.delta(llm.Delta{Reasoning: d.Thinking})
	case "input_json_delta":
		i, ok := c.toolOfBlock[ev.Index]
		if !ok {
			return
		}

		t := c.tools[i]
		t.fragments.WriteString(d.PartialJSON)
		m.delta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: i, ID: t.id, Name: m.toolName(t.name), ArgsFragment: d.PartialJSON}})
	}
}

func (m *mapper) onUser(msg *apiMessage) {
	if msg == nil {
		return
	}

	var results []contentBlock

	for _, b := range blocks(msg.Content) {
		if b.Type == "tool_result" {
			results = append(results, b)
		}
	}

	if len(results) == 0 {
		return
	}

	m.closeCall()

	for _, r := range results {
		text := toolResultText(r.Content)
		m.messages = append(m.messages, llm.Message{Role: "tool", ToolCallID: m.maskSecret(r.ToolUseID), Content: m.mask(text)})

		if r.IsError {
			m.res.ToolCallFailures++
			m.emit(events.ToolResult, map[string]any{"id": r.ToolUseID, "error": m.mask(text)})

			continue
		}

		m.emit(events.ToolResult, map[string]any{"id": r.ToolUseID, "output_len": len(text)})
	}
}

func (m *mapper) onRateLimit(info *rateLimitInfo) {
	rl := parseRateLimit(info)
	m.lastRL = rl

	// Keep the last known windows: an event without any says nothing new.
	if len(rl.Windows) > 0 {
		m.subscription = slices.Clone(rl.Windows)
	}

	if m.o.OnRateLimit != nil {
		m.o.OnRateLimit(rl)
	}

	if rl.Status == "rejected" {
		m.limit = m.usageLimit(rl)
		m.halted = true
	}
}

func (m *mapper) usageLimit(rl RateLimit) *UsageLimitError {
	return &UsageLimitError{Window: m.mask(rl.Window), ResetsAt: rl.ResetsAt}
}

// closeCall emits the open call's events in harness order.
func (m *mapper) closeCall() {
	c := m.open
	if c == nil {
		return
	}

	m.open = nil
	m.closed[c.id] = true

	content := strings.Join(c.text, "\n\n")
	m.lastText = content

	if thinking := strings.Join(c.thinking, "\n\n"); thinking != "" {
		m.emit(events.Thinking, map[string]any{"turn": c.turn, "content": m.mask(thinking)})
	}

	finishReason := c.stopReason
	if finishReason == "" {
		finishReason = c.lineStop
	}

	m.emit(events.ModelResponse, map[string]any{
		"turn": c.turn, "content": m.mask(content), "content_len": len(content),
		"tool_calls": len(c.tools), "finish_reason": finishReason, "model": c.model,
	})

	u := c.usage()
	m.streamed[c.model] = m.streamed[c.model].add(u)
	m.streamedTotal = m.streamedTotal.add(u)
	m.emitUsage(c.model, u)

	m.messages = append(m.messages, m.assistantMessage(content, c.tools))

	for _, t := range c.tools {
		m.res.ToolCallCount++
		m.emit(events.ToolCallKind, map[string]any{"id": t.id, "name": m.toolName(t.name), "raw_args": m.mask(t.args())})
	}
}

func (m *mapper) emitUsage(model string, u tokenBuckets) {
	data := map[string]any{
		"prompt_tokens": u.prompt, "completion_tokens": u.completion,
		"cache_read_tokens": u.cacheRead, "cache_creation_tokens": u.cacheCreation,
		"model": model, "cost_basis": protocol.CostBasisSubscription,
	}

	if len(m.subscription) > 0 {
		data["subscription"] = m.subscription
	}

	m.emit(events.UsageKind, data)
}

func (m *mapper) onResult(l streamLine) {
	m.closeCall()

	m.sawResult = true

	// modelUsage and total_cost_usd are cumulative over a resumed session (and
	// a Session's earlier turns); this process's or turn's share is here.
	perModel := l.ModelUsageSinceLastResult
	for _, u := range perModel {
		m.res.TotalCostUSD += u.CostUSD
	}

	if perModel == nil && !m.o.Session {
		perModel = l.ModelUsage
		m.res.TotalCostUSD = l.TotalCostUSD
	}

	// Calls Claude Code made without streaming them (compaction, for one) show
	// up only here; one event per model carries the difference.
	for _, model := range slices.Sorted(maps.Keys(perModel)) {
		u := perModel[model]
		total := tokenBuckets{u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens}

		if rest := total.remainder(m.streamed[model]); rest != (tokenBuckets{}) {
			m.emitUsage(model, rest)
		}
	}

	m.res.Turns = l.NumTurns
	m.res.PromptTokens = deref(l.Usage.InputTokens)
	m.res.CompletionTokens = deref(l.Usage.OutputTokens)
	m.res.CacheReadTokens = deref(l.Usage.CacheReadInputTokens)
	m.res.CacheCreationTokens = deref(l.Usage.CacheCreationInputTokens)
	m.res.Output = m.mask(l.Result)

	reason, err := m.outcome(l)
	m.conclude(reason, err)
}

// outcome judges success by is_error, never subtype: an auth failure reports
// subtype success.
func (m *mapper) outcome(l streamLine) (string, error) {
	switch {
	case m.authFailed || (l.IsError && strings.HasPrefix(l.Result, "Not logged in")):
		return "error", m.notLoggedIn(l.Result)
	case l.IsError && l.APIErrorStatus != nil && *l.APIErrorStatus == 429 &&
		strings.Contains(strings.ToLower(l.Result), "usage limit"):
		m.limit = m.usageLimit(m.lastRL)

		return "error", m.limit
	case l.Subtype == "error_max_turns":
		return "max_turns", nil
	case l.Subtype == "error_max_budget_usd":
		return "max_cost", nil
	case !l.IsError:
		// terminal_reason is not consulted, so a release that drops or renames
		// it degrades instead of failing every run.
		return "done", nil
	}

	detail := l.Result
	if detail == "" {
		detail = strings.Join(l.Errors, "; ")
	}

	if detail == "" {
		detail = fmt.Sprintf("%s (terminal reason %q)", l.Subtype, l.TerminalReason)
	}

	return "error", fmt.Errorf("claudecode: run failed: %s", m.mask(detail))
}

// conclude records the outcome and emits the closing events: error (for
// Reason "error"), then a Run's single stop or a Session turn's awaiting_human.
// A Session's stop comes from the Session when its process ends.
func (m *mapper) conclude(reason string, err error) {
	m.res.Reason = reason
	m.res.Messages = slices.Clone(m.messages)
	m.res.Completed = reason == "done"
	m.res.ModelUsed = m.maskSecret(m.res.ModelUsed)
	m.err = m.annotate(reason, err)

	if m.res.Completed && m.o.Terminal != nil {
		if _, args, ok := m.o.Terminal(); ok {
			m.res.CompletionArgs = json.RawMessage(m.maskSecret(string(args)))
		}
	}

	if m.limit != nil {
		data := map[string]any{"window": m.limit.Window}
		if !m.limit.ResetsAt.IsZero() {
			data["resets_at"] = m.limit.ResetsAt.Format(time.RFC3339)
		}

		m.emit(events.Kind("usage_limit"), data)
	} else if reason == "error" && m.err != nil {
		m.emit(events.ErrorKind, map[string]any{"error": m.err.Error()})
	}

	switch {
	case !m.o.Session:
		m.emit(events.StateChange, map[string]any{"stop": reason, "turns": m.res.Turns})
	case m.sawResult:
		m.emit(events.StateChange, map[string]any{"state": "awaiting_human", "turns": m.res.Turns})
	}
}

// annotate names both versions on an error from an untested release. Typed
// outcomes (login, usage limit) and cancellation need no version context.
func (m *mapper) annotate(reason string, err error) error {
	var ule *UsageLimitError

	if err == nil || reason != "error" || m.version == "" || m.version == Version ||
		errors.Is(err, ErrNotLoggedIn) || errors.As(err, &ule) {
		return err
	}

	return &maskedError{text: err.Error() + m.mask(fmt.Sprintf(" (claude %s, tested %s)", m.version, Version)), err: err}
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}
