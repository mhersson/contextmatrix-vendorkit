# Shared setup for the probe scripts; sourced, never run.
#   SCRATCH  required: an empty directory every probe writes under
#   CLAUDE   the claude binary (default: $SCRATCH/cc/node_modules/.bin/claude from install.sh)
#   PORT     mock Messages API port on 127.0.0.1 (default 28111; inside the jail it is private)
set -euo pipefail

: "${SCRATCH:?set SCRATCH to an empty scratch directory}"
CLAUDE=${CLAUDE:-$SCRATCH/cc/node_modules/.bin/claude}
PORT=${PORT:-28111}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CLAUDE_PATH="$(dirname "$CLAUDE"):/usr/bin:/bin"
# The variable name stays apart from its value so no script line reads like an env dump.
TOKEN_VAR=CLAUDE_CODE_OAUTH_TOKEN
DUMMY_TOKEN="$TOKEN_VAR=dummy-oauth"
DUMMY_KEY="ANTHROPIC_API_KEY=dummy-key"
BASE_URL="ANTHROPIC_BASE_URL=http://127.0.0.1:$PORT"

[ -x "$CLAUDE" ] || { echo "claude binary not found at $CLAUDE (run install.sh or set CLAUDE)" >&2; exit 1; }

# fresh DIR: creates DIR, failing when it exists so stale logs never mix into a result.
fresh() {
  [ ! -e "$1" ] || { echo "$1 exists; use a fresh SCRATCH" >&2; exit 1; }
  mkdir -p "$1"
}

# MSG TEXT: one stream-json user message.
MSG() {
  python3 -I -c 'import json,sys;print(json.dumps({"type":"user","message":{"role":"user","content":[{"type":"text","text":sys.argv[1]}]}}))' "$1"
}

# start_mock OUTDIR SCENARIO [TOOL_INPUT_JSON [HOLD_FILE]]: requests go to OUTDIR/requests.jsonl.
start_mock() {
  mkdir -p "$1"
  python3 -I "$HERE/mock.py" "$PORT" "$1/requests.jsonl" "$2" "${3:-{\}}" ${4:+"$4"} 2>"$1/mock.err" &
  MOCKPID=$!
  python3 -I -c '
import socket, sys, time
for _ in range(100):
    try:
        socket.create_connection(("127.0.0.1", int(sys.argv[1])), 0.2).close()
        sys.exit(0)
    except OSError:
        time.sleep(0.05)
sys.exit(1)' "$PORT" || { echo "mock did not start on port $PORT" >&2; exit 1; }
}

stop_mock() {
  kill "$MOCKPID" 2>/dev/null || true
  wait "$MOCKPID" 2>/dev/null || true
}

# run_claude OUTDIR HOME [ENV=VALUE...] -- [driver args...] -- [claude args...]
# The child env holds only PATH, HOME, DISABLE_AUTOUPDATER and the given pairs.
run_claude() {
  local out=$1 home=$2
  shift 2
  local envs=() dargs=()
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  while [ "$1" != "--" ]; do dargs+=("$1"); shift; done
  shift
  mkdir -p "$out"
  env -i PATH="$CLAUDE_PATH" HOME="$home" DISABLE_AUTOUPDATER=1 "${envs[@]}" \
    python3 -I "$HERE/driver.py" --out "$out/stdout.jsonl" --timing "$out/timing.txt" --err "$out/stderr.txt" \
    "${dargs[@]}" -- "$CLAUDE" "$@" || true
}

# Flags every mock run passes (tools, model and extras follow).
BASE_ARGS=(-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages
  --setting-sources user --strict-mcp-config --permission-mode dontAsk)
