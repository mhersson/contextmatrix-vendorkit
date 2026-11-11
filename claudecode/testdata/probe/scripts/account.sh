#!/bin/bash
# account.sh (inside jail.sh): the initialize answer's account object under env sets that
# move a run off the subscription token, and which credential a request carries when the
# OAuth token and an API key are both set. Writes $SCRATCH/mock/account.
source "$(dirname "$0")/lib.sh"
A=$SCRATCH/mock/account
fresh "$A"
mkdir -p "$A/work"
cd "$A/work"
start_mock "$A" text
ARGS=("${BASE_ARGS[@]}" --tools "" --model claude-haiku-5-5)
one() { # NAME ENV...: initialize only
  local name=$1
  shift
  mkdir -p "$A/$name/home" "$A/$name/cfg"
  run_claude "$A/$name" "$A/$name/home" CLAUDE_CONFIG_DIR="$A/$name/cfg" "$@" -- --gate --close 3 -- "${ARGS[@]}"
}
one oauth "$DUMMY_TOKEN"
one oauth-key "$DUMMY_TOKEN" "$DUMMY_KEY"
one oauth-bedrock "$DUMMY_TOKEN" CLAUDE_CODE_USE_BEDROCK=1
one oauth-mantle "$DUMMY_TOKEN" CLAUDE_CODE_USE_MANTLE=1
one oauth-simple "$DUMMY_TOKEN" CLAUDE_CODE_SIMPLE=1
one oauth-socket "$DUMMY_TOKEN" ANTHROPIC_UNIX_SOCKET="$A/none.sock"
stop_mock

# One message with both credentials; the mock records the headers it got.
start_mock "$A/both" text
mkdir -p "$A/both/home" "$A/both/cfg"
run_claude "$A/both" "$A/both/home" CLAUDE_CONFIG_DIR="$A/both/cfg" "$DUMMY_TOKEN" "$DUMMY_KEY" "$BASE_URL" -- \
  --gate --send "0:$(MSG 'hello')" --close 30 --close-on-result 1 -- "${ARGS[@]}"
stop_mock
