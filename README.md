# contextmatrix-vendorkit

Runs vendor coding-agent CLIs as ContextMatrix executors for
`contextmatrix-agent`, `contextmatrix-chat` and `contextmatrix-architect`. The
first adapter, `claudecode`, drives the unmodified Claude Code CLI in headless
mode so the backends can run on a Claude subscription.

## Packages

- `claudecode` - builds and supervises a headless Claude Code run and reports
  it through the caller's `events.Emitter`.
- `mcpbridge` - exposes ContextMatrix MCP tools to the CLI.
- `subproc` - child-process supervision shared by vendor adapters.

## Using claudecode

```go
if found, tested, err := claudecode.CheckVersion(ctx, binary); err == nil && !tested {
    // log one warning naming found and claudecode.Version; carry on
}
```

Check the version once at start. A mismatch is never an error and a run on an
untested release proceeds; if it fails, the error names the version that ran.

**Run** (agent phases, architect stages): split the harness registry, set the
`Spec` and call `Run`.

```go
builtins, bridged := claudecode.SplitRegistry(reg) // read/write/edit/bash -> built-ins
res, err := claudecode.Run(ctx, claudecode.Spec{
    Dir: dir, Home: home, OAuthToken: token, Model: model, Task: task,
    BuiltinTools: builtins, Tools: bridged, Emitter: emitter,
    Effort: claudecode.NormalizeEffort(configured),
})
```

**Session** (chat): `StartSession` returns once Claude Code has answered the
`initialize` request; `Send` blocks until that turn's result; `Close` ends the
process. `ID()` is known when `StartSession` returns (`SessionID`, `Resume`, or
a UUID vendorkit generates), so store it after every start. Never call `Close`
or `Send` from `OnDelta`, `OnRateLimit` or an Emitter writer: they run
mid-turn and deadlock.

Errors to branch on:

- `*UsageLimitError` (`errors.As`): a subscription usage limit stopped the run
  or ended the session. Park or show the state; never retry.
- `ErrNotLoggedIn` (`errors.Is`): Claude Code did not accept the token.
- Cancellation: the error wraps `context.Cause(ctx)` and `Result.Reason` is
  `"canceled"`. A cancel that arrives after the result line keeps that
  result's outcome.

Success is judged by `is_error`, never `subtype`. An unknown or absent
terminal reason with `is_error: false` is `done`.

### Event contract

Events are the harness kinds, in harness order. The ones that differ:

- `usage`: one per API call, plus a per-model remainder at the result for
  calls Claude Code made without streaming them. Always
  `cost_basis: "subscription"`; never `cost_usd` and no notional cost. The
  Result's tokens and `TotalCostUSD` come from the per-model usage
  (`model_usage_since_last_result`, else `modelUsage`), so a resumed run counts
  only its own process.
- `usage_limit` (`window`, `resets_at` when known): emitted before the final
  `state_change` when a usage limit stops the run or session.

## Claude Code invariants

- The isolation flags are always on, so a run never reads the host user's
  Claude Code settings, hooks or memory. `--bare` is never passed. Every
  invocation also passes `--system-prompt-snapshot off`, so a resumed start
  runs its own system prompt.
- Readiness is the stream-json `initialize` control_request/control_response,
  for both `Run` and `Session`; the first message is written only after it.
  When the answer carries an `account`, the start fails closed unless it
  reports `tokenSource: "CLAUDE_CODE_OAUTH_TOKEN"`, `apiProvider:
  "firstParty"` and no API key source; the error names what Claude Code
  reported. An answer without `account` proceeds.
- Stopping a run goes through an http stop hook on the loopback bridge, not
  signals alone.
- The bridge's `--mcp-config` entry carries a per-server `timeout` of 1e8 ms.
  Without it 2.1.322 cuts a bridged call at 60 s, since the bridge answers only
  when the tool returns.
- Process lifecycle: the CLI runs in its own process group. 2.1.322's Bash
  tool starts children with `setsid`, which leaves that group; on Linux
  `subproc` kills such escaped descendants before the SIGKILL escalation. On
  other unix systems only the group kill and Claude Code's own cleanup apply.
- State lives under `Home/.vendorkit`: `run-*` directories (one per process
  start, holding the generated config) and `sessions/` (the transcript store).
  The Claude Code config dir is fresh per process start; a resume restores
  only the stored transcript into it.
- The child environment is scrubbed of every variable 2.1.322 reads that
  moves a run off the subscription token: API keys and endpoints, the
  `CLAUDE_CODE_USE_*` provider switches, host-managed provider settings and
  `CLAUDE_CODE_SIMPLE` (the env form of `--bare`). The OAuth token is masked in
  every event, Result field, error, stderr tail and stored transcript.

## Tested version

`claudecode.Version` is the release this module is tested against, not a hard
pin. Images ContextMatrix builds install it. A host install belongs to the
user: nothing here installs, upgrades or changes it. See section "Claude Code
rules" in `AGENTS.md`.

Moving `Version`:

1. Record new fixtures and re-run the probes in
   `claudecode/testdata/probe/scripts/` (procedure in
   `claudecode/testdata/README.md`); update the decisions table.
2. Run the smoke tests, which drive the real CLI through `Run` against a mock
   API with dummy credentials. They skip unless `VENDORKIT_SMOKE_CLAUDE` (the
   binary) and `VENDORKIT_SMOKE_MOCK` (the mock's base URL) are set;
   `smoke.sh` sets both and must run inside `jail.sh`, a network namespace
   with only loopback up:

   ```bash
   go test -c -o "$SCRATCH/claudecode.test" ./claudecode
   SMOKE_TEST="$SCRATCH/claudecode.test" claudecode/testdata/probe/scripts/jail.sh \
     claudecode/testdata/probe/scripts/smoke.sh
   ```

3. The operator does a live auth check.

## Development

See `AGENTS.md`. Verify with
`make fmt && make test && make test-race && make lint && make deps-gate && make build`.
