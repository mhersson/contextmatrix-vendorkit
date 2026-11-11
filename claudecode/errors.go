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

// ErrNotLoggedIn reports that Claude Code did not authenticate with the token.
var ErrNotLoggedIn = errors.New("claudecode: not logged in")
