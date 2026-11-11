// Package claudecode runs the Claude Code CLI in headless mode as a
// ContextMatrix executor.
//
// Run executes one unit of work (a phase, a stage, a review seat) as one
// `claude -p` process and returns a harness-shaped Result. StartSession keeps
// one process alive for a conversation (chat) and answers it turn by turn with
// Send. Both report through Spec.Emitter in the harness event vocabulary:
//
//   - model_request, thinking, model_response, usage, tool_call and
//     tool_result per API call; user_input and state_change awaiting_human per
//     Session turn; error; one state_change stop per Run or Session.
//   - usage: one event per API call, plus one per model for calls Claude Code
//     made without streaming them (so the events sum to the result). It
//     carries cost_basis "subscription" and never cost_usd, because Claude
//     Code's cost is a list-price estimate, not billed money.
//   - usage_limit: emitted when a subscription usage limit stops the run or
//     session, which then fails with *UsageLimitError.
//
// The process starts in its own process group with a scrubbed environment and
// a fresh CLAUDE_CONFIG_DIR under Spec.Home/.vendorkit, so it never reads the
// host user's Claude Code settings, hooks or memory. A persisted session's
// transcript lives in Spec.Home/.vendorkit/sessions and is restored into the
// fresh config dir on resume. Every invocation passes
// --system-prompt-snapshot off, so a resumed start runs its own system
// prompt. Readiness is the stream-json initialize control_response; the task
// is written only after it, and only when the account that answer reports
// (if any) is the OAuth token on the first-party API.
//
// Errors to branch on: *UsageLimitError (park or show the state, never
// retry), ErrNotLoggedIn (the token was not accepted), and an error wrapping
// context.Cause(ctx) with Reason "canceled". Success is judged by is_error,
// never by subtype. The OAuth token is masked in every event, Result field,
// error, stderr tail and stored transcript.
//
// CheckVersion compares the installed CLI with Version, the tested release.
package claudecode
