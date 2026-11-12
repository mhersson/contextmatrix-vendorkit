# AGENTS.md - ContextMatrix VendorKit

Runs vendor coding-agent CLIs (Claude Code today) as ContextMatrix executors
for `contextmatrix-agent`, `contextmatrix-chat` and `contextmatrix-architect`.
This file is the contributor contract; `README.md` holds the design overview.

## Boundary discipline

The harness forbids external dependencies and backendkit has its own import
allowlist, so the vendor executor lives here.

- Allowed imports: `contextmatrix-protocol`, `contextmatrix-harness/harness`,
  `contextmatrix-harness/events`, `contextmatrix-harness/llm`,
  `contextmatrix-harness/tools`, `modelcontextprotocol/go-sdk/mcp`, stdlib, and
  `testify` in tests only. Forbidden: `contextmatrix`, `-agent`, `-chat`,
  `-architect`, `-backendkit`, and any other external module.
- CI (`make deps-gate`, `scripts/deps-gate.sh`) rejects any forbidden repo
  dependency and any direct import in non-test code outside the allowlist
  above. An import that trips the gate means fix the design, not the gate.

## Package map

- `claudecode` - builds, runs and supervises a headless Claude Code session.
- `mcpbridge` - serves the backend's tool registry to the CLI on a loopback MCP
  server, with the stop hook (CM's own MCP server rides `Spec.MCPServers`).
- `subproc` - child-process supervision shared by vendor adapters.
- `internal/fakeclaude` - fake CLI binary used by tests.

## Coding conventions

- Go 1.27+; format with `make fmt` (gofumpt); imports via goimports.
- Wrap errors with `fmt.Errorf("operation: %w", err)`. Never swallow an error.
- Exported entry points that block on I/O (process, network, files) take
  `context.Context` first: `Run`, `StartSession`, `Session.Send`,
  `CheckVersion`, `mcpbridge.Start`. The exceptions are `subproc` `Start`,
  `Stop` and `Wait`, `Bridge.Close`, `Session.Close` and the unexported
  transcript helpers.
- No global state, no `init()`. Dependencies are struct or `Spec` fields wired
  by the caller.
- Interfaces live in the package that uses them; constructors and entry points
  return concrete types. Backends that need a seam define their own interface
  over `Run` and `Session`.
- Tests sit next to code, table-driven, `t.TempDir()`, `t.Helper()` in helpers;
  `testify/require` for fatal checks, `testify/assert` otherwise.
- Logging: the module logs nothing itself; it reports through the caller's
  `events.Emitter`, returned errors and callbacks. Any log line added later uses
  `log/slog` with structured fields, never `fmt.Println`.
- Lint is strict and `make lint` is the authority.

## Claude Code rules

- `claudecode.Version` is the tested release, not a hard pin. Images
  ContextMatrix builds install it. A host install is the user's: no
  ContextMatrix tool installs, upgrades or changes it. `CheckVersion` reports a
  mismatch for the caller to log as a warning, and a run on an untested release
  proceeds (if it fails after init reported a version other than `Version`, the
  error names both, login and usage-limit errors aside; a failure before init,
  such as a rejected flag, carries no version).
- Moving `Version` is a tested change: new fixtures, the CLI probes re-run, the
  opt-in smoke tests (`VENDORKIT_SMOKE_CLAUDE`, `VENDORKIT_SMOKE_MOCK`, run via
  `claudecode/testdata/probe/scripts/smoke.sh` inside `jail.sh`) and
  a live auth check by the operator.
- A release that makes `--bare` the `-p` default ignores the OAuth token and
  fails loudly as `ErrNotLoggedIn`; the operator decides how to move past it
  when it happens.
- Never pass `--bare`. The isolation flags are always on, and every invocation
  passes `--system-prompt-snapshot off`.
- Never call `Session.Close` or `Session.Send` from `OnDelta`, `OnRateLimit` or
  an Emitter writer: the session calls them mid-turn, so it deadlocks.
- The OAuth token is masked in everything the module emits or returns. No real
  token in tests, fixtures or CI: tests use the fake binary and every fixture
  passes the token scan.

## Tests

- Tests must exercise behavior. A test that only fails when someone rewords a
  string, renames a heading, or edits prose is not a test - do not write it, and
  delete it when found. Asserting exact text is legitimate only where the text is
  a wire contract a consumer parses; if no code reads it, no test should pin it.

## Verification

```bash
make fmt && make test && make test-race && make lint && make deps-gate && make build
```

## Documentation

- Documentation must always be true: every statement matches the code at
  HEAD, and a change that makes a doc line false updates it in the same
  commit.
- Document the current state - what exists now and why, not how it got here.
- Comments explain only non-obvious decisions, constraints, safety invariants,
  or workarounds. Do not narrate what code does or record change history; git
  holds history. Keep comments to one or two tight lines unless a longer
  explanation is genuinely necessary. Rewrite or delete if you find comments
  that don't follow this rule.
- Never use em-dashes; use hyphens (-).
- Never reference plan phases, task numbers, or private card IDs in doc comments.

## Commit discipline

Run the Verification set above before every commit.

- **Never commit to `main`.** Work on a feature branch; tasks of a plan or
  scope the user approved commit there without asking again. Pushing a
  feature branch and opening a PR need no approval; merging a PR, pushing
  to `main` and pushing a tag each need explicit user approval.
- Conventional commits: `type(scope): concise summary`. Always include a scope.
- Body uses bullet points for the what and why - no long paragraphs.
- Never reference plan phases, task numbers, or private card IDs in messages.
- No session links in commits or PRs.
