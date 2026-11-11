# claudecode testdata

Stream-json fixtures and CLI probe captures for Claude Code 2.1.322, the
release `claudecode.Version` names. `TestFixturesCarryNoToken` scans every file
here for credential shapes.

- `real/` - CLI output recorded on 2026-11-11 (see below for the one edit).
- `synthetic/` - shapes the recorded runs do not cover, derived from a real
  fixture or captured against a mock Messages API.
- `probe/` - captures that decide the flag set, the readiness gate and the
  config-dir isolation (the decisions table below), and in `probe/scripts/`
  the scripts that produce them.

## Tool and server names

The prototype's MCP server was named `cmlocal`, so recorded tool calls read
`mcp__cmlocal__finish` (`t5`, `t6`, `p3`, `p3b`) where production names the
bridge server `cm` (`mcp__cm__finish`). Synthetic fixtures derived from `t5`
keep `cmlocal`. Mapper tests replaying any of them strip the prefix
`mcp__cmlocal__`.

## real/

| File | Recorded | Shows |
|---|---|---|
| `t1.jsonl` | live, `claude-haiku-5-5`, default tools | one text call: `init`, `assistant`, `rate_limit_event` (allowed, `seven_day`), `result` |
| `t2.jsonl` | live, authentication failed | `subtype: "success"` with `is_error: true`, result `Not logged in · Please run /login`, `terminal_reason: "api_error"`; a `system/ui_invalidate` line before `init` |
| `t3-iso.jsonl` | live, isolation flags | one `Bash` (`echo hi`) tool turn |
| `t5.jsonl` | live, `claude-sonnet-5-5`, `Read` plus `cmlocal` finish | two calls: text and the finish `tool_use` under one `message.id`, then `Done.`; `result.usage` 4/125/3844/3977 (input/output/cache read/cache creation) |
| `t6.jsonl` | live, http `PostToolUse` stop hook | ends with `terminal_reason: "hook_stopped"` right after finish |
| `t7.jsonl` | live, stream-json input | the message written at process start was dropped; only the second was answered |
| `t7b.jsonl` | live, stream-json input, second message after a delay | two turns, each with its own `init` and `result`; replayed `user` lines (`isReplay`) |
| `p4-partial-messages.jsonl` | live, `claude-haiku-5-5`, `--include-partial-messages` | `stream_event` lines (`message_start`, `content_block_*`, `message_delta`, `message_stop`) and a `system/status` line |
| `p3.jsonl` | mock Messages API | finish refused by MCP schema validation (`isError`), then accepted; the refusal fires `PostToolUseFailure` only |
| `p3b.jsonl` | mock Messages API | as `p3`, refused by a handler error |
| `dl-httphook.jsonl` | mock Messages API, distroless image | an http `PostToolUse` hook ends the run with `hook_stopped` |

`p3`, `p3b` and `dl-httphook` come from the prototype's `mock-api/` directory.

The only edit to these captures: in `p3.jsonl` and `p3b.jsonl` the
prototype's local scratch directory is rewritten to `/scratch/spike`, and its
dash-encoded `projects/` key to `-scratch-spike` (`cwd`, `original_cwd`,
`memory_paths`). Every other byte is as recorded.

## synthetic/

| File | Derived from | Change |
|---|---|---|
| `partial.jsonl` | `real/t5.jsonl`, in the shapes of `real/p4-partial-messages.jsonl` | `stream_event` lines added per call: `message_start` (all four buckets; the early `output_tokens` of the call's assistant line), text deltas in fragments, a `tool_use` block start with three `input_json_delta` fragments that concatenate to the finish input, `message_delta` with cumulative complete usage and `stop_reason` (call 1: input 2, output 120, cache read 0, cache creation 3844, `tool_use`; call 2: 2, 5, 3844, 133, `end_turn`), `message_stop`. The `t5` lines are unchanged; the `message_delta` usages sum to `result.usage`. Call 1's early `output_tokens` (2) differs from its final 120 |
| `rejected.jsonl` | `real/t1.jsonl` | `rate_limit_event` status `rejected` (window `seven_day` rejected, utilization 1.0); no `result` line. A real rejection is unrecorded |
| `max_turns.jsonl` | CLI output of a mock-API run, `--max-turns 1`, one `Bash` turn, gated by `initialize` | `result` with `subtype: "error_max_turns"`, `is_error: true`, `terminal_reason: "max_turns"`, `errors: ["Reached maximum number of turns (1)"]` and no `result` field; the process exited 1. Starts with the gate's `control_response`; usage values are the mock's |
| `two_models.jsonl` | `real/t5.jsonl` | `result.modelUsage` and `model_usage_since_last_result` gain a `claude-haiku-5-5` entry (350/12/0/0, `costUSD` 0.000041) that no assistant message served; `total_cost_usd` 0.0175914; `result.usage` unchanged |
| `unknown.jsonl` | `real/t1.jsonl` | an unknown top-level type (`x_future_event`), an unknown `system` subtype (`x_future_subtype`), `x_future_field` on every known line, and unknown keys inside the assistant `message` and `result.usage` |
| `crash.jsonl` | `real/t5.jsonl` | the `result` line removed |

## probe/

The scratch directory the probes ran in is replaced by `/scratch` (and its
`projects/` key by `-scratch`) in these captures; nothing else is edited.

- `version.txt`, `help.txt`: `claude --version` and `claude --help`.
- `startup.jsonl`: `claude -p --input-format stream-json --output-format
  stream-json --verbose --include-partial-messages` with stdin open and
  nothing written for 10 s, then closed. It is empty: no line before input,
  exit 0 at 10.2 s.
- `handshake.jsonl`: the same flags; `{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}`
  written at start, stdout recorded for 10 s. One `control_response`
  (`subtype: "success"`, `request_id: "r1"`, `account.tokenSource: "none"`)
  at 0.20 s, then nothing.
- `p6/`: probe P6 (config-dir isolation and resume). `run-{a,b,c}.jsonl` is
  each run's stdout, `mock-requests-{a,b,c}.jsonl` each request the mock
  received (authorization header, parameters, messages, system prompt length,
  hash and markers).
- `snapshot/`: the requests of the system-prompt resume probe (runs a, b, c
  of `scripts/snapshot.sh`).

How they ran (`probe/scripts/`):

- CLI: `install.sh`, which runs `npm install --prefix $SCRATCH/cc
  @anthropic-ai/claude-code@2.1.322` and then the package's `install.cjs`.
- No-credential captures: `noenv.sh`, under `env -i
  PATH=<claude dir>:/usr/bin:/bin HOME=$SCRATCH/home`, cwd an empty scratch
  directory.
- Mock-API runs: `bash-and-permissions.sh`, `flags.sh`, `gate.sh`,
  `max-turns.sh`, `p6.sh` and `snapshot.sh`, each inside `jail.sh` (a network
  namespace with only loopback up, so no request can leave the host), against
  `mock.py`. The child env is `env -i` plus `PATH`, `HOME`,
  `DISABLE_AUTOUPDATER=1`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_BASE_URL` and either
  `ANTHROPIC_API_KEY=dummy-key` or a dummy OAuth token (P6 and the snapshot
  probe). Flags: `-p --input-format stream-json --output-format stream-json
  --verbose --include-partial-messages --setting-sources user
  --strict-mcp-config --permission-mode dontAsk --tools <list> --model
  claude-haiku-5-5` (effort runs: `claude-sonnet-5-5`); the user message
  follows the `initialize` gate except in `gate.sh`'s ungated runs.

## Decisions for 2.1.322

| Decision | Value | Evidence |
|---|---|---|
| `PermissionMode` | `dontAsk` | `help.txt:151-154` lists `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` (`default` is not a choice; `init` reports `default` when the flag is absent). Mock, `--tools Bash --allowedTools Bash`: `echo allowed-ran` ran, `tool_result_meta` `permission_decision` `accept`/`config`/`rule`. Mock, `--tools Bash` without `--allowedTools`: `touch <file>` was refused at once (`tool_result` `is_error: true`, "Permission to use Bash has been denied because Claude Code is running in don't ask mode"), listed in `result.permission_denials`, file not created, run `completed`. A read-only command (`echo readonly-ran`) ran without an allow rule |
| `EffortFlag` | `--effort <level>` | `help.txt:84-85`; mock requests carry the value as `output_config.effort` |
| `SystemPromptFlags` | replace `--system-prompt-file <path>`, append `--append-system-prompt-file <path>` | `help.txt` lists only the inline forms (`:26`, `:232`) and names the file forms in the `--bare` text (`:55-56`). Mock: with the replace file the request `system` is a billing-header line, `You are a Claude agent, built on Anthropic's Claude Agent SDK.` and the file content (191 chars); with the append file it is the default prompt (about 8.6k chars) followed by the file content. Either way the environment section arrives as a `role: "system"` entry in `messages` |
| `NoPersistenceFlag` | `--no-session-persistence` | `help.txt:143-145`. Mock with stream-json input: no `projects/` transcript written with the flag; one written without it |
| `ReadinessSignal` | `control_response` | `probe/handshake.jsonl`: `control_response` to `r1` at 0.20 s. `probe/startup.jsonl`: no `init` before input (`init` appears only after a user message, once per turn) |
| `RunStdinGate` | yes | `real/t7.jsonl`: a message written at start was dropped once, live. Mock: 30 of 30 ungated single-message runs and 5 of 5 runs writing two messages at start were answered, so the drop did not reproduce against the mock; the `initialize` request written at start was answered in every gated run (39 of 39 in the pass that produced these captures, 0.20 to 0.23 s). The gate costs one round trip and removes the only observed failure |
| `UsageLimitWaitSetting` | none | `help.txt` names no such flag or setting. In the 2.1.322 binary the usage-limit wait runs only under the opt-in retry watchdog: `ni=qTe()&&ylt(Ao)` with `function qTe(){return i.CLAUDE_CODE_RETRY_WATCHDOG}` (cap `CLAUDE_CODE_RETRY_WATCHDOG_MAX_USAGE_LIMIT_WAIT_MS`); without it a retry-after past the cap throws `api_request_retry_after_too_long`. The child env already drops `CLAUDE_CODE_RETRY_WATCHDOG` |
| `BashTimeoutEnv` | `BASH_DEFAULT_TIMEOUT_MS` yes; `BASH_MAX_TIMEOUT_MS` yes, floored at the effective default | `grep -a -c`: 4 each. Binary: max = `Math.max(BASH_MAX_TIMEOUT_MS, default)`, default = `BASH_DEFAULT_TIMEOUT_MS` or 120000. Mock, `sleep 5`: `BASH_DEFAULT_TIMEOUT_MS=2000`, no `timeout` -> `"Exit code 143\nCommand timed out after 2s"` (`is_error: true`) 2.0 s after the `tool_use`; `BASH_MAX_TIMEOUT_MS=2000` alone, `timeout: 10000` -> `(Bash completed with no output)` after 5.0 s, as with no env; `BASH_DEFAULT_TIMEOUT_MS=1000 BASH_MAX_TIMEOUT_MS=2000`, `timeout: 10000` -> `"Exit code 143\nCommand timed out after 2s"` after 2.0 s. A max below 120 s takes effect only when `BASH_DEFAULT_TIMEOUT_MS` at or below it is set too |
| `EffortValues` | `low`, `medium`, `high`, `xhigh`, `max` | `help.txt:84-85`; `handshake.jsonl` `models[].supportedEffortLevels` is the same list for every model. Mock: each value reached the request as `output_config.effort`. `bogus` and `minimal` are not rejected: stderr warns `Unknown --effort value 'bogus'`, says it is ignored for the default and lists the five values, the run proceeds and the request carries the default `medium` (also the value with no `--effort`) |
| `ConfigDirIsolation` | pass; no file besides the transcript | `probe/p6/`. Every request carried `authorization: Bearer dummy-oauth` and no `x-api-key`. Run B (fresh config dir holding only A's `projects/<key>/<uuid>.jsonl`, `--resume <uuid>`): its first request carries A's user message, `tool_use` `echo run-a`, its `tool_result` and `done`, then B's message; same session ID. Hooks written while A waited on the mock into A's config dir, `HOME/.claude/settings.json` and the store dir never fired; the control hook in C's config dir before start fired. `HOME` held only the planted `.claude/settings.json`. Not copied and not needed: `.claude.json`, `backups/`, `plugins/store/`, `telemetry/`. Auto memory resolves to `<config dir>/projects/<key>/memory/` |
| `SystemPromptSnapshot` | `--system-prompt-snapshot off` on every invocation | Listed: `help.txt:233-248` (default `on`: the first request's prompt is recorded and reused on every later request and resume). `probe/snapshot/`, resuming as P6 does from a fresh config dir holding only the copied transcript: A (`--session-id`, append file `MARKER-APPEND`) and B (`--resume`, a different append file) both sent A's prompt (`system_markers` `MARKER-APPEND`); C (as B plus `--system-prompt-snapshot off`) sent its own (`MARKER-REPLACE`). The transcript holds the snapshot (`attachment` and `api-request-shape` entries), so with `on` the transcript would carry instructions into a later start |

## Recorded findings

- `BASH_MAX_TIMEOUT_MS` below 120000 needs `BASH_DEFAULT_TIMEOUT_MS` too: the
  CLI takes the larger of the two (default 120000 when unset), so a max alone
  below the default changes nothing (`BashTimeoutEnv` row).
- `--max-turns` is accepted but hidden (`help.txt` does not list it), and an
  `error_max_turns` run exits 1 after its `result` line
  (`synthetic/max_turns.jsonl`).
- `rate_limit_event` against the mock, which sends no rate-limit headers,
  lacks `resetsAt` and the window fields (`rateLimitType`,
  `unifiedWindows`):
  `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","isUsingOverage":false}}`
  (`probe/p6/run-a.jsonl`, OAuth).
- `--max-budget-usd` (`help.txt:131`) and `--replay-user-messages`
  (`help.txt:191`) are listed.
- `init` reports a `messaging_socket_path` under `/tmp/cc-socks/`: the CLI
  writes there, outside `HOME` and the config dir.

## Re-running the probes when moving Version

Needs bash, python3, node with npm, and util-linux `unshare` with
`--map-user` on a kernel that allows unprivileged user namespaces. No token:
the only credentials are the dummy ones the scripts set.

```bash
export SCRATCH=$(mktemp -d)                      # every probe writes here; must start empty
VERSION=<new release> claudecode/testdata/probe/scripts/install.sh
claudecode/testdata/probe/scripts/run-all.sh     # all probes, then summary.py
```

`CLAUDE=<path to a claude binary>` skips `install.sh`; `PORT` moves the mock
(default 28111, private to the jail). Compare the summary with the decisions
table and update each row that changed, then rewrite `probe/` and
`synthetic/` from the new captures with `python3 -I
claudecode/testdata/probe/scripts/build-fixtures.py "$SCRATCH"
claudecode/testdata` (or set `BUILD=1` on the `run-all.sh` run). `real/`
needs new live recordings with a token, made by the operator. Finish with
`go test ./claudecode/ -run TestFixturesCarryNoToken`.
