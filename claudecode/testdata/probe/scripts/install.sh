#!/bin/bash
# install.sh: installs Claude Code $VERSION (default 2.1.322) into $SCRATCH/cc. npm 12 skips
# the package's postinstall, so install.cjs runs explicitly.
set -euo pipefail
: "${SCRATCH:?set SCRATCH to an empty scratch directory}"
VERSION=${VERSION:-2.1.322}
npm install --prefix "$SCRATCH/cc" "@anthropic-ai/claude-code@$VERSION"
node "$SCRATCH/cc/node_modules/@anthropic-ai/claude-code/install.cjs"
"$SCRATCH/cc/node_modules/.bin/claude" --version
