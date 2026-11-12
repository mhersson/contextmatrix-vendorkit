package claudecode

import (
	"errors"
	"fmt"
	"time"
)

// UsageLimitError reports that a subscription usage limit stopped the run or
// turn. Callers park (agent) or show the state (chat, architect); never retry.
type UsageLimitError struct {
	Window   string
	ResetsAt time.Time // zero when the vendor reported none
}

func (e *UsageLimitError) Error() string {
	msg := "claudecode: subscription usage limit reached"
	if e.Window != "" {
		msg += " (" + e.Window + ")"
	}

	if !e.ResetsAt.IsZero() {
		msg += fmt.Sprintf(", resets at %s", e.ResetsAt.UTC().Format(time.RFC3339))
	}

	return msg
}

// MCPServerError reports that Claude Code marked a configured MCP server
// (a Spec.MCPServers entry) failed or needs-auth at init. The halted turn's
// request may already be out; a Session discards that turn and keeps the stored
// transcript from the last completed turn, so a caller may resume and resend it
// once. A halt on a fresh session's first turn leaves nothing stored, so start a
// fresh session.
type MCPServerError struct {
	Server string // the Spec.MCPServers name
	Status string // "failed" or "needs-auth", as init reported it
}

func (e *MCPServerError) Error() string {
	return fmt.Sprintf("claudecode: MCP server %q reported %s", e.Server, e.Status)
}

// ErrNotLoggedIn reports that Claude Code did not authenticate with the token.
var ErrNotLoggedIn = errors.New("claudecode: not logged in")
