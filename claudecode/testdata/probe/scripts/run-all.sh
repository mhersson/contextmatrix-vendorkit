#!/bin/bash
# run-all.sh: every probe into a fresh $SCRATCH (after install.sh), then the summary.
# BUILD=1 also rewrites probe/ and synthetic/ in this testdata from the new captures.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
: "${SCRATCH:?set SCRATCH to an empty scratch directory}"
"$HERE/noenv.sh"
for s in bash-and-permissions background flags gate max-budget max-turns parallel-tools p6 snapshot account attribution; do
  "$HERE/jail.sh" "$HERE/$s.sh"
done
for set in short sixty; do
  IDLE_CASES=$set "$HERE/jail.sh" "$HERE/idle-timeout.sh"
done
python3 -I "$HERE/summary.py" "$SCRATCH"
if [ "${BUILD:-0}" = 1 ]; then
  python3 -I "$HERE/build-fixtures.py" "$SCRATCH" "$HERE/../.."
fi
