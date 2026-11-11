package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"

	"github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
	"github.com/mhersson/contextmatrix-vendorkit/subproc"
)

// serverName is the bridge's MCP server name, so the model sees mcp__cm__<tool>.
const serverName = "cm"

// bridgeTimeoutMS lifts 2.1.322's 60 s http MCP request timeout and its idle
// watchdog for the bridge, which answers only when a tool returns (agent verify
// runs 10 min), to the CLI's default 1e8 ms call cap; Run's ctx bounds tools.
const bridgeTimeoutMS = 100_000_000

// droppedEnv are removed from the child env: a stray API key, endpoint or
// provider switch silently overrides the subscription token. The names are the
// ones 2.1.322 reads; checkAccount catches any it adds later.
var droppedEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET",
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
	"CLAUDE_CODE_SIMPLE",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_RETRY_WATCHDOG",
}

type bridgeInfo struct {
	Server    mcpbridge.ServerConfig
	HookURL   string
	Tools     []string // qualified names of every bridged tool
	Terminals []string // qualified names of the terminal tools
}

type mcpServerEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int64             `json:"timeout,omitempty"` // ms
}

type hookEntry struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type settingsFile struct {
	Hooks map[string][]hookMatcher `json:"hooks,omitempty"`
}

// buildInvocation returns the claude argv and the per-run file contents keyed
// by absolute path under runDir. It writes nothing; the caller writes the files
// at 0600. Neither the task, a system prompt nor the token appears in argv.
//
//nolint:unparam // the caller passes a per-run directory
func buildInvocation(s Spec, bi bridgeInfo, runDir string, session bool) (args []string, files map[string][]byte, err error) {
	if err := s.validate(session); err != nil {
		return nil, nil, err
	}

	mcpPath := filepath.Join(runDir, "mcp.json")
	settingsPath := filepath.Join(runDir, "settings.json")
	files = map[string][]byte{}

	servers := map[string]mcpServerEntry{}
	if bi.Server.URL != "" {
		servers[serverName] = mcpServerEntry{Type: "http", URL: bi.Server.URL, Headers: bi.Server.Headers, Timeout: bridgeTimeoutMS}
	}

	for _, m := range s.MCPServers {
		servers[m.Name] = mcpServerEntry{Type: "http", URL: m.URL, Headers: m.Headers}
	}

	if files[mcpPath], err = json.Marshal(map[string]any{"mcpServers": servers}); err != nil {
		return nil, nil, fmt.Errorf("marshal mcp config: %w", err)
	}

	settings := settingsFile{}

	if len(bi.Terminals) > 0 {
		quoted := make([]string, len(bi.Terminals))
		for i, name := range bi.Terminals {
			quoted[i] = regexp.QuoteMeta(name)
		}

		settings.Hooks = map[string][]hookMatcher{"PostToolUse": {{
			Matcher: "^(" + strings.Join(quoted, "|") + ")$",
			Hooks:   []hookEntry{{Type: "http", URL: bi.HookURL}},
		}}}
	}

	if files[settingsPath], err = json.Marshal(settings); err != nil {
		return nil, nil, fmt.Errorf("marshal settings: %w", err)
	}

	args = []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--system-prompt-snapshot", "off",
		"--permission-mode", "dontAsk",
		"--model", s.Model,
		"--mcp-config", mcpPath,
		"--settings", settingsPath,
		"--tools", strings.Join(s.BuiltinTools, ","),
	}

	allowed := append([]string(nil), s.BuiltinTools...)
	allowed = append(allowed, bi.Tools...)

	for _, m := range s.MCPServers {
		allowed = append(allowed, "mcp__"+m.Name)
	}

	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}

	switch {
	case s.SystemPrompt != "":
		p := filepath.Join(runDir, "system-prompt.txt")
		files[p] = []byte(s.SystemPrompt)
		args = append(args, "--system-prompt-file", p)
	case s.SystemPromptAppend != "":
		p := filepath.Join(runDir, "system-prompt-append.txt")
		files[p] = []byte(s.SystemPromptAppend)
		args = append(args, "--append-system-prompt-file", p)
	}

	switch {
	case s.SessionID != "":
		args = append(args, "--session-id", s.SessionID)
	case s.Resume != "":
		args = append(args, "--resume", s.Resume)
	case !session:
		args = append(args, "--no-session-persistence")
	}

	if s.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(s.MaxTurns))
	}

	if s.MaxBudgetUSD > 0 && !session {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(s.MaxBudgetUSD, 'f', -1, 64))
	}

	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}

	return args, files, nil
}

// childEnv builds the claude process env from Spec.Env.
func childEnv(s Spec, configDir string) []string {
	return subproc.BuildEnv(s.Env, droppedEnv, map[string]string{
		"HOME":                    s.Home,
		"CLAUDE_CONFIG_DIR":       configDir,
		"CLAUDE_CODE_OAUTH_TOKEN": s.OAuthToken,
		"DISABLE_AUTOUPDATER":     "1",
	})
}

type inputBlock struct {
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
	Source *inputImage `json:"source,omitempty"`
}

type inputImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// userMessage renders one stream-json user line, newline-terminated. Images
// with no text go without a text block, which would carry no text field.
func userMessage(text string, images []llm.ImageURL) ([]byte, error) {
	var blocks []inputBlock
	if text != "" || len(images) == 0 {
		blocks = append(blocks, inputBlock{Type: "text", Text: text})
	}

	for _, img := range images {
		rest, ok := strings.CutPrefix(img.URL, "data:")
		if !ok {
			return nil, errors.New("claudecode: image is not a data URL")
		}

		meta, data, ok := strings.Cut(rest, ",")
		media, isB64 := strings.CutSuffix(meta, ";base64")

		if !ok || !isB64 || media == "" || data == "" {
			return nil, errors.New("claudecode: malformed base64 image data URL")
		}

		blocks = append(blocks, inputBlock{Type: "image", Source: &inputImage{Type: "base64", MediaType: media, Data: data}})
	}

	type body struct {
		Role    string       `json:"role"`
		Content []inputBlock `json:"content"`
	}

	msg := struct {
		Type    string `json:"type"`
		Message body   `json:"message"`
	}{Type: "user", Message: body{Role: "user", Content: blocks}}

	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal user message: %w", err)
	}

	return append(b, '\n'), nil
}
