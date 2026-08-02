#!/usr/bin/env bash
# Bootstrap a development environment for Genesis AI Factory.
#
# The Go toolchain is installed into the workspace rather than the system, so
# the environment is reproducible and needs no root. Module and build caches go
# alongside it for the same reason.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.25.5}"
TOOLCHAIN_DIR="${TOOLCHAIN_DIR:-$HOME/toolchain}"

if command -v go >/dev/null 2>&1 && go version | grep -qE "go1\.(2[5-9]|[3-9][0-9])"; then
  echo "✔ $(go version)"
else
  echo "→ installing Go ${GO_VERSION} into ${TOOLCHAIN_DIR}"
  mkdir -p "${TOOLCHAIN_DIR}"
  curl -sSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C "${TOOLCHAIN_DIR}" -xz
  export PATH="${TOOLCHAIN_DIR}/go/bin:${PATH}"
  echo "✔ $(go version)"
  echo
  echo "  Add this to your shell profile:"
  echo "    export PATH=\"${TOOLCHAIN_DIR}/go/bin:\$PATH\""
fi

go env -w GOMODCACHE="$HOME/.gocache/mod" GOCACHE="$HOME/.gocache/build" GOTOOLCHAIN=local

echo "→ resolving Go dependencies"
go work sync

if command -v npm >/dev/null 2>&1; then
  echo "→ installing desktop dependencies"
  (cd apps/desktop && npm install --no-audit --no-fund --silent)
else
  echo "! npm not found; skipping the desktop app"
fi

echo
echo "✔ ready. Next:"
echo "    make build      # compile the server and CLI"
echo "    make test       # run every suite"
echo "    make models     # see which local models fit this machine"
