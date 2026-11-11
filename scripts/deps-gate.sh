#!/usr/bin/env bash
# Enforces the vendorkit boundary invariant:
#   1. the module depends on no backend, server or backendkit repo
#   2. harness usage stays within harness/events/llm/tools
#   3. non-test code directly imports only the stdlib, this module, protocol,
#      those harness packages and the go-sdk mcp package
# Transitive dependencies of the allowed modules are not listed; keeping them
# out is review-enforced. A failing `go list` fails the gate; it never passes
# vacuously.
set -euo pipefail

cd "$(dirname "$0")/.."

mod="github.com/mhersson/contextmatrix-vendorkit"

# Standalone assignments so set -e aborts when go list itself fails.
all_deps=$(go list -deps -test ./...)
bad_repo=$(printf '%s\n' "${all_deps}" \
  | grep -E 'mhersson/(contextmatrix$|contextmatrix/|contextmatrix-agent|contextmatrix-chat|contextmatrix-architect|contextmatrix-backendkit)' || true)
if [ -n "${bad_repo}" ]; then
  echo "FAIL: forbidden repo dependency:" >&2
  echo "${bad_repo}" >&2
  exit 1
fi

bad_harness=$(printf '%s\n' "${all_deps}" \
  | grep '^github.com/mhersson/contextmatrix-harness/' \
  | grep -vE '/(harness|events|llm|tools)$' || true)
if [ -n "${bad_harness}" ]; then
  echo "FAIL: harness import outside {harness,events,llm,tools}:" >&2
  echo "${bad_harness}" >&2
  exit 1
fi

# .Imports excludes test imports, so testify in a _test.go file stays allowed.
direct=$(go list -f '{{range .Imports}}{{.}}{{"\n"}}{{end}}' ./...)
bad_ext=$(printf '%s\n' "${direct}" \
  | grep -v '^$' \
  | grep -v '^[^/.]*\(/\|$\)' \
  | grep -vE "^${mod}(/|$)" \
  | grep -vE '^github.com/mhersson/contextmatrix-protocol$' \
  | grep -vE '^github.com/mhersson/contextmatrix-harness/(harness|events|llm|tools)$' \
  | grep -vE '^github.com/modelcontextprotocol/go-sdk/mcp$' \
  | sort -u || true)
if [ -n "${bad_ext}" ]; then
  echo "FAIL: import outside the allowlist in non-test code:" >&2
  echo "${bad_ext}" >&2
  exit 1
fi

echo "deps-gate: ok"
