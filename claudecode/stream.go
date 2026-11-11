package claudecode

import (
	"encoding/json"
	"errors"
	"strings"
)

// streamLine holds the fields the mapper reads from any stream-json line;
// encoding/json skips the rest.
type streamLine struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	IsReplay bool   `json:"isReplay"`

	Error string `json:"error"` // assistant: authentication_failed, ...

	// system/init
	SessionID         string      `json:"session_id"`
	ClaudeCodeVersion string      `json:"claude_code_version"`
	MCPServers        []mcpServer `json:"mcp_servers"`

	Message *apiMessage `json:"message"` // assistant, user

	Event        *streamEvent `json:"event"` // stream_event
	APIMessageID string       `json:"api_message_id"`

	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"` // rate_limit_event

	// result
	IsError                   bool                  `json:"is_error"`
	APIErrorStatus            *int                  `json:"api_error_status"`
	Result                    string                `json:"result"`
	Errors                    []string              `json:"errors"`
	NumTurns                  int                   `json:"num_turns"`
	TerminalReason            string                `json:"terminal_reason"`
	TotalCostUSD              float64               `json:"total_cost_usd"`
	Usage                     apiUsage              `json:"usage"`
	ModelUsage                map[string]modelUsage `json:"modelUsage"`
	ModelUsageSinceLastResult map[string]modelUsage `json:"model_usage_since_last_result"`
}

type mcpServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type apiMessage struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"` // a string for plain user text, else blocks
	StopReason string          `json:"stop_reason"`
	Usage      apiUsage        `json:"usage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // tool_result: a string or text blocks
}

type streamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *apiMessage   `json:"message"`       // message_start
	ContentBlock *contentBlock `json:"content_block"` // content_block_start
	Delta        *eventDelta   `json:"delta"`
	Usage        *apiUsage     `json:"usage"` // message_delta
}

type eventDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// apiUsage keeps absent buckets nil so a later source can fill them.
type apiUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

type modelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type rateLimitInfo struct {
	Status         string                     `json:"status"`
	ResetsAt       *float64                   `json:"resetsAt"`
	RateLimitType  string                     `json:"rateLimitType"`
	UnifiedWindows map[string]rateLimitWindow `json:"unifiedWindows"`
}

type rateLimitWindow struct {
	Utilization float64  `json:"utilization"`
	ResetsAt    *float64 `json:"resetsAt"`
	Status      string   `json:"status"`
}

// decodeLine reports false for a line that is not a JSON object. A known field
// whose type changed is left zero instead of dropping the line, so a newer
// release degrades rather than failing the run.
func decodeLine(b []byte) (streamLine, bool) {
	var l streamLine

	err := json.Unmarshal(b, &l)

	var typeErr *json.UnmarshalTypeError
	if err != nil && !errors.As(err, &typeErr) {
		return streamLine{}, false
	}

	return l, true
}

// blocks decodes a content array; plain-string content yields none.
func blocks(raw json.RawMessage) []contentBlock {
	var out []contentBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}

// toolResultText flattens tool_result content: a string, or text blocks joined.
func toolResultText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []string

	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}

	return strings.Join(parts, "\n")
}
