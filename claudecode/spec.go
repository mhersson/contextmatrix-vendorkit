package claudecode

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"

	"github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
)

// effortValues are the --effort levels Version accepts.
func effortValues() []string { return []string{"low", "medium", "high", "xhigh", "max"} }

// Spec configures one Run or Session process.
type Spec struct {
	Binary     string   // "" = "claude" on PATH
	Dir        string   // working directory of the run
	Home       string   // HOME for the commands the Bash built-in runs; required; may be long-lived and shared by concurrent runs; vendorkit keeps its own state under Home/.vendorkit and touches nothing else
	OAuthToken string   // becomes CLAUDE_CODE_OAUTH_TOKEN in the child env only; required
	Env        []string // base env (KEY=VALUE) for claude and the commands its Bash tool runs, minus the variables that move a run off the subscription; nil = only what vendorkit sets (HOME, CLAUDE_CONFIG_DIR, the token, DISABLE_AUTOUPDATER), so no PATH: pass at least PATH. A proxy variable here also adds the loopback hosts to NO_PROXY and no_proxy

	Model      string         // Claude Code model ID
	Task       string         // first user message, written to stdin, never argv (Run only)
	TaskImages []llm.ImageURL // data URLs, sent as image blocks with Task

	// At most one of the two system-prompt fields is set. SystemPrompt
	// replaces Claude Code's system prompt; SystemPromptAppend appends to it.
	// Both go through a file, never argv.
	SystemPrompt       string
	SystemPromptAppend string

	BuiltinTools []string                 // Claude Code built-ins (--tools); empty = none (`--tools ""`)
	Tools        *tools.Registry          // served on the loopback bridge; nil = none
	MCPServers   []mcpbridge.ServerConfig // extra servers beside the bridge (chat: CM's MCP)

	MaxTurns     int     // --max-turns; 0 = CLI default
	MaxBudgetUSD float64 // --max-budget-usd, on notional cost; 0 = none
	Effort       string  // "" = CLI default; else a value NormalizeEffort returns unchanged (validation fails otherwise)

	SessionID string // new persisted session with this UUID
	Resume    string // continue this session ID (same Home and Dir); at most one of SessionID/Resume; neither = no persistence for Run, a persisted session with a fresh ID for Session

	Emitter *events.Emitter     // required; events follow the README "Event contract"
	Redact  func(string) string // applied to content, args, thinking and errors before Emit; nil = identity
	// OnDelta receives every streamed fragment synchronously, in order: text
	// (Content), thinking (Reasoning) and tool input (ToolCall: Index, ID,
	// bare Name, Arguments fragment). It is the liveness signal the agent's
	// idle watchdog reads and the source of the architect's drafts. Like
	// harness.Run, no fragment arrives while a tool runs. Fragments are raw
	// model output: Redact is not applied. nil = no callback.
	OnDelta     func(llm.Delta)
	OnRateLimit func(RateLimit) // every rate_limit_event, in order; nil = ignored
}

// NormalizeEffort trims and lowercases e and returns it when Version accepts it
// for --effort; otherwise it returns "", the CLI default. No aliasing:
// "minimal" or a typo becomes "". Every backend passes its configured effort
// through it before setting Spec.Effort, and logs once when a non-empty value
// comes back "".
func NormalizeEffort(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	if slices.Contains(effortValues(), e) {
		return e
	}

	return ""
}

func (s Spec) validate(session bool) error {
	switch {
	case s.OAuthToken == "":
		return errors.New("claudecode: Spec.OAuthToken is required")
	case s.Home == "":
		return errors.New("claudecode: Spec.Home is required")
	case s.Emitter == nil:
		return errors.New("claudecode: Spec.Emitter is required")
	case s.Model == "":
		return errors.New("claudecode: Spec.Model is required")
	case s.SystemPrompt != "" && s.SystemPromptAppend != "":
		return errors.New("claudecode: at most one of Spec.SystemPrompt and Spec.SystemPromptAppend may be set")
	case s.SessionID != "" && s.Resume != "":
		return errors.New("claudecode: at most one of Spec.SessionID and Spec.Resume may be set")
	case s.Effort != "" && NormalizeEffort(s.Effort) != s.Effort:
		return fmt.Errorf("claudecode: Spec.Effort %q is not one of %v", s.Effort, effortValues())
	case session && s.Task != "":
		return errors.New("claudecode: Spec.Task is unused by a Session; send it with Send")
	}

	// The ID names a transcript file and is matched in a glob.
	for field, id := range map[string]string{"SessionID": s.SessionID, "Resume": s.Resume} {
		if id != "" && !sessionIDPattern.MatchString(id) {
			return fmt.Errorf("claudecode: Spec.%s must hold only letters, digits, '-' and '_'", field)
		}
	}

	seen := map[string]bool{}

	for _, m := range s.MCPServers {
		switch {
		case m.Name == "":
			return errors.New("claudecode: Spec.MCPServers entry has no name")
		case m.Name == serverName:
			return fmt.Errorf("claudecode: Spec.MCPServers entry name %q is reserved for the tool bridge", serverName)
		case seen[m.Name]:
			return fmt.Errorf("claudecode: Spec.MCPServers name %q appears more than once", m.Name)
		}

		seen[m.Name] = true
	}

	return nil
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
