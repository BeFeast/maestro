#!/usr/bin/env bash
# Install the latest maestro release binary.
# Usage: curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/scripts/install.sh | bash
set -euo pipefail

REPO="BeFeast/maestro"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
if [ -z "$LATEST" ]; then
  echo "error: could not determine latest release" >&2
  exit 1
fi

BINARY="maestro-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/download/${LATEST}/${BINARY}"

echo "Downloading ${BINARY} ${LATEST}..."
curl -fsSL "${URL}" -o "${INSTALL_DIR}/maestro"
chmod +x "${INSTALL_DIR}/maestro"
echo "Maestro ${LATEST} installed to ${INSTALL_DIR}/maestro"
