#!/bin/bash
# smoke.sh (inside jail.sh): claudecode's TestSmoke* tests, the real claude through Run and
# Session against mock.py's smoke scenario. SMOKE_TEST is a test binary from
# `go test -c -o <path> ./claudecode`. Writes $SCRATCH/smoke.
source "$(dirname "$0")/lib.sh"
: "${SMOKE_TEST:?set SMOKE_TEST to a binary built with go test -c ./claudecode}"
S=$SCRATCH/smoke
fresh "$S"
start_mock "$S" smoke
cd "$HERE/../../.."
rc=0
VENDORKIT_SMOKE_CLAUDE="$CLAUDE" VENDORKIT_SMOKE_MOCK="http://127.0.0.1:$PORT" \
  "$SMOKE_TEST" -test.run '^TestSmoke' -test.v -test.count=1 >"$S/test.txt" 2>&1 || rc=$?
stop_mock
cat "$S/test.txt"
exit "$rc"
