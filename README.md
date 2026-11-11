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

## Claude Code invariants

- The isolation flags are always on, so a run never reads the host user's
  Claude Code settings, hooks or memory. `--bare` is never passed.
- Stopping a run goes through an http stop hook, not signals alone.
- The child environment is scrubbed, and the OAuth token is masked in
  everything the module emits or returns.

## Tested version

`claudecode.Version` is the release this module is tested against, not a hard
pin. Images ContextMatrix builds install it. A host install belongs to the
user: nothing here installs, upgrades or changes it. See section "Claude Code
rules" in `AGENTS.md`.

## Development

See `AGENTS.md`. Verify with
`make fmt && make test && make test-race && make lint && make deps-gate && make build`.
