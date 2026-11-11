package claudecode

import "strings"

const redactedSecret = "[REDACTED]"

// newMask returns the function every emitted or returned string passes:
// redact (nil = identity), then the secret replaced. The secret is also
// replaced before redact, so a rule rewriting part of it cannot leave the rest.
func newMask(secret string, redact func(string) string) func(string) string {
	return func(s string) string {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, redactedSecret)
		}

		if redact != nil {
			s = redact(s)

			if secret != "" {
				s = strings.ReplaceAll(s, secret, redactedSecret)
			}
		}

		return s
	}
}

// maskedError keeps errors.Is and errors.As working on err while its text
// passes the mask.
type maskedError struct {
	text string
	err  error
}

func (e *maskedError) Error() string { return e.text }

func (e *maskedError) Unwrap() error { return e.err }
