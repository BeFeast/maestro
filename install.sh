#!/bin/sh
# Install the latest maestro release binary from GitHub Releases.
# Usage: curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | sh
set -eu

REPO="BeFeast/maestro"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

main() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64)  ARCH="amd64" ;;
        aarch64) ARCH="arm64" ;;
        arm64)   ARCH="arm64" ;;
        *)
            echo "error: unsupported architecture: $ARCH" >&2
            exit 1
            ;;
    esac

    case "$OS" in
        linux|darwin) ;;
        *)
            echo "error: unsupported OS: $OS" >&2
            exit 1
            ;;
    esac

    LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
    if [ -z "$LATEST" ]; then
        echo "error: could not determine latest release" >&2
        exit 1
    fi

    BINARY="maestro-${OS}-${ARCH}.tar.gz"
    URL="https://github.com/${REPO}/releases/download/${LATEST}/${BINARY}"
    TMPDIR=$(mktemp -d)
    TMPFILE="${TMPDIR}/maestro-${OS}-${ARCH}.tar.gz"
    trap 'rm -rf "$TMPDIR"' EXIT

    echo "Downloading maestro ${LATEST} (${OS}/${ARCH})..."
    if ! curl -fsSL "$URL" -o "$TMPFILE"; then
        echo "error: failed to download ${URL}" >&2
        echo "Check available binaries at https://github.com/${REPO}/releases/latest" >&2
        exit 1
    fi
    tar xzf "$TMPFILE" -C "$TMPDIR"
    BINARY_PATH="${TMPDIR}/maestro-${OS}-${ARCH}"
    chmod +x "$BINARY_PATH"

    if [ -w "$INSTALL_DIR" ]; then
        mv "$BINARY_PATH" "${INSTALL_DIR}/maestro"
    else
        echo "Installing to ${INSTALL_DIR} (requires sudo)..."
        sudo mv "$BINARY_PATH" "${INSTALL_DIR}/maestro"
    fi

    echo "maestro ${LATEST} installed to ${INSTALL_DIR}/maestro"

    # Verify the installed binary works
    if "${INSTALL_DIR}/maestro" version >/dev/null 2>&1; then
        echo "Verified: $("${INSTALL_DIR}/maestro" version)"
    fi
}

main
