#!/usr/bin/env bash
# Install Bolt for end users (native binary; no container required).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/saurabhahuja71/boltgo/main/scripts/install.sh | bash
#   INSTALL_DIR=~/bin ./scripts/install.sh
#   AGENTERM_VERSION=v0.1.0 ./scripts/install.sh
#   AGENTERM_FROM_SOURCE=1 ./scripts/install.sh   # build with local Go toolchain
#
# Env:
#   AGENTERM_REPO        release repository (default saurabhahuja71/boltgo)
#   AGENTERM_VERSION     default latest (GitHub release tag or "latest")
#   INSTALL_DIR          default ~/.local/bin
#   AGENTERM_FROM_SOURCE if 1/true, skip release download and build from source
#   AGENTERM_SKIP_INIT   if 1/true, do not create ~/.agenterm/config.toml
set -euo pipefail

REPO="${AGENTERM_REPO:-saurabhahuja71/boltgo}"
VERSION="${AGENTERM_VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"
FROM_SOURCE="${AGENTERM_FROM_SOURCE:-0}"
SKIP_INIT="${AGENTERM_SKIP_INIT:-0}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  msys*|mingw*|cygwin*) os=windows ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac

ext=""
[[ "$os" == windows ]] && ext=".exe"
asset="agenterm-${os}-${arch}${ext}"
dest="${INSTALL_DIR}/bolt${ext}"

mkdir -p "$INSTALL_DIR"

download_release() {
  local url tmp code
  if [[ "$VERSION" == latest ]]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi
  tmp="$(mktemp)"
  echo "Downloading ${url}"
  code="$(curl -fsSL -o "$tmp" -w '%{http_code}' "$url" || true)"
  if [[ "$code" != "200" ]]; then
    rm -f "$tmp"
    echo "Release download failed (HTTP ${code:-error}) for ${asset}" >&2
    return 1
  fi
  chmod +x "$tmp"
  mv "$tmp" "$dest"
  echo "Installed ${dest}"
}

build_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    echo "Go toolchain not found; cannot build from source." >&2
    echo "Install Go (https://go.dev/dl/) or use a GitHub Release binary." >&2
    return 1
  fi

  local src work
  # Prefer current repo if we are already inside agenterm sources.
  if [[ -f "./go.mod" ]] && grep -q 'module github.com/saurabhahuja71/agenterm' ./go.mod 2>/dev/null; then
    src="$(pwd)"
    echo "Building from current directory: ${src}"
    (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$dest" ./cmd/agenterm)
  else
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' RETURN
    echo "Cloning https://github.com/${REPO}.git …"
    git clone --depth 1 "https://github.com/${REPO}.git" "$work/agenterm"
    (cd "$work/agenterm" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$dest" ./cmd/agenterm)
  fi
  chmod +x "$dest"
  echo "Installed ${dest} (from source)"
}

installed=0
if [[ "$FROM_SOURCE" == "1" || "$FROM_SOURCE" == "true" ]]; then
  build_from_source
  installed=1
else
  if download_release; then
    installed=1
  else
    echo "Falling back to build-from-source…"
    build_from_source
    installed=1
  fi
fi

if [[ "$installed" != "1" ]]; then
  echo "install failed" >&2
  exit 1
fi

# Keep the historical executable name while making Bolt the primary command.
# Symlinks also preserve argv[0], so the launcher presets select bolt-s1/s2/s3.
if [[ "$os" != "windows" ]]; then
  for launcher in agenterm bolt-s1 bolt-s2 bolt-s3; do
    ln -sfn "$(basename "$dest")" "${INSTALL_DIR}/${launcher}"
  done
  echo "Installed launchers: bolt, bolt-s1, bolt-s2, bolt-s3 (agenterm compatibility alias)"
fi

# PATH hint
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "Note: ${INSTALL_DIR} is not on your PATH."
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    echo "Add that line to ~/.bashrc or ~/.zshrc for permanent use."
    ;;
esac

# Default config (new users only; never overwrite without --force)
if [[ "$SKIP_INIT" != "1" && "$SKIP_INIT" != "true" ]]; then
  if [[ ! -f "${HOME}/.agenterm/config.toml" ]]; then
    echo ""
    echo "Creating default config…"
    if "$dest" init 2>/dev/null; then
      :
    else
      # Binary may be too old for init tips; still try
      "$dest" init || true
    fi
  else
    echo ""
    echo "Config already present: ${HOME}/.agenterm/config.toml"
    echo "  Refresh defaults (tools/prompt fix):  bolt init --force"
  fi
fi

echo ""
echo "Next steps:"
echo "  1. Ensure a backend is reachable (local or SSH tunnel):"
echo "       Ollama:  curl -s http://127.0.0.1:11434/v1/models | head"
echo "       SGLang:  curl -s http://127.0.0.1:30000/v1/models | head"
echo "  2. Ping Bolt:"
echo "       bolt --ping"
echo "       bolt --provider sglang --ping"
echo "  3. Chat:"
echo "       bolt"
echo "       bolt -m qwen2.5-coder:32b"
echo "       bolt --provider sglang"
echo "       bolt --no-tools          # pure chat, fastest replies"
echo "  4. In TUI (during chat, like Grok):"
echo "       /help"
echo "       /model                       # list models on the server"
echo "       /model qwen2.5-coder:32b     # switch model mid-session"
echo "       /tools off                   # faster replies"
echo ""
echo "Done."
