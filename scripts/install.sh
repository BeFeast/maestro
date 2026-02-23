#!/bin/bash
# Install the latest maestro binary for your platform.
# Usage: curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/scripts/install.sh | bash
set -euo pipefail

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')
BINARY="maestro-${OS}-${ARCH}"

LATEST=$(curl -fsSL https://api.github.com/repos/BeFeast/maestro/releases/latest | grep tag_name | cut -d'"' -f4)
if [ -z "$LATEST" ]; then
  echo "Error: could not determine latest release" >&2
  exit 1
fi

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

echo "Downloading ${BINARY} ${LATEST}..."
curl -fsSL "https://github.com/BeFeast/maestro/releases/download/${LATEST}/${BINARY}" -o "${INSTALL_DIR}/maestro"
chmod +x "${INSTALL_DIR}/maestro"
echo "Maestro ${LATEST} installed to ${INSTALL_DIR}/maestro"
