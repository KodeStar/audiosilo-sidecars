#!/usr/bin/env bash
# Build the web SPA and embed it into the daemon binary.
#
# The frontend build output (web/dist) lives at the repo root, which go:embed
# cannot reach from internal/web. This script builds the SPA, syncs it into
# internal/web/dist (gitignored), and compiles the binary with `-tags embedui`
# so the real UI is baked in. The default `go build` (no tag) embeds the tiny
# placeholder in internal/web/dist-placeholder instead, so it always works
# without Node.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Node 24 (workspace convention).
if [ -d "$HOME/.nvm/versions/node/v24.16.0/bin" ]; then
  export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH"
fi

echo ">> node $(node --version 2>/dev/null || echo 'NOT FOUND')"

echo ">> building web/ ..."
(cd web && npm ci && npm run build)

EMBED_DIR="internal/web/dist"
echo ">> syncing web/dist -> $EMBED_DIR ..."
rm -rf "$EMBED_DIR"
mkdir -p "$EMBED_DIR"
cp -R web/dist/. "$EMBED_DIR"/

echo ">> building bin/audiosilo-sidecars (-tags embedui) ..."
mkdir -p bin
go build -tags embedui -o bin/audiosilo-sidecars ./cmd/audiosilo-sidecars

echo ">> done. Run: ./bin/audiosilo-sidecars serve"
