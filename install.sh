#!/bin/bash
# unity-sync installer. Downloads the latest release binary for your platform into
# ~/.local/bin. No credential is needed; a GITHUB_TOKEN, GH_TOKEN or gh login is used when
# present only to get GitHub's authenticated API rate limit.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/curbol/unity-sync/main/install.sh | bash
set -euo pipefail

REPO="curbol/unity-sync"
BINARY_NAME="unity-sync"
INSTALL_DIR="${HOME}/.local/bin"

log()  { printf 'INFO: %s\n' "$1"; }
err()  { printf 'ERROR: %s\n' "$1" >&2; }

TMPDIR_SELF=""
STAGED=""
cleanup() {
  [[ -z "$TMPDIR_SELF" ]] || rm -rf "$TMPDIR_SELF"
  [[ -z "$STAGED" ]] || rm -f "$STAGED"
}
trap cleanup EXIT

auth_token() {
  local token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "$token" ]] && command -v gh >/dev/null 2>&1; then
    token=$(gh auth token 2>/dev/null || true)
  fi
  printf '%s' "$token"
}

# fetch GETs a URL, passing the credential through a curl config on stdin rather than as an
# argument, which would put the token in `ps` output for every local user. It is optional:
# with no token the request goes out unauthenticated, which the API serves. Callers append
# `|| true` where they want to report the failure themselves: every one of them assigns from
# a pipeline, and `set -e` plus `pipefail` would otherwise abort before the diagnostic runs.
fetch() {
  local url="$1"; shift
  local token; token=$(auth_token)
  if [[ -n "$token" ]]; then
    printf 'header = "Authorization: token %s"\n' "$token" | curl -fsSL -K - "$@" "$url"
  else
    curl -fsSL "$@" "$url"
  fi
}

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin*) os="mac" ;;
    Linux*)  os="linux" ;;
    *) err "unsupported OS $(uname -s); on Windows use the release zip directly"; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="intel" ;;
    arm64|aarch64) [[ "$os" == "mac" ]] && arch="apple" || arch="arm64" ;;
    *) err "unsupported arch $(uname -m)"; exit 1 ;;
  esac
  PLATFORM="${os}-${arch}"
  log "platform: $PLATFORM"
}

latest_version() {
  VERSION=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
  VERSION=${VERSION#v}
  [[ -n "$VERSION" ]] || { err "could not resolve the latest version from the GitHub API"; exit 1; }
  log "latest version: $VERSION"
}

install() {
  local file="${BINARY_NAME}-${VERSION}-${PLATFORM}.zip"
  TMPDIR_SELF=$(mktemp -d)
  local tmp="$TMPDIR_SELF"

  # The browser download URL serves everyone, so there is one code path rather than a
  # token-only branch that had to find the asset's API url by grepping pretty-printed JSON.
  curl -fsSL -o "${tmp}/${file}" "https://github.com/${REPO}/releases/download/v${VERSION}/${file}" \
    || { err "could not download ${file}"; exit 1; }

  command -v unzip >/dev/null 2>&1 || { err "unzip is required"; exit 1; }
  unzip -q "${tmp}/${file}" -d "$tmp"
  mkdir -p "$INSTALL_DIR"

  # Staged inside the install dir so the last step is a same-filesystem rename. mktemp -d
  # lands in /tmp, usually a different filesystem, where mv degrades to copy-then-unlink
  # and an interrupted upgrade leaves a truncated binary in place of a working one.
  STAGED=$(mktemp "${INSTALL_DIR}/.${BINARY_NAME}.XXXXXX")
  cp "${tmp}/${BINARY_NAME}" "$STAGED"
  # Set, not `chmod +x`: mktemp makes the file 0600, so adding the execute bits alone
  # would install 0711 and strip read access from group and other.
  chmod 0755 "$STAGED"
  mv -f "$STAGED" "${INSTALL_DIR}/${BINARY_NAME}"
  STAGED=""
  log "installed to ${INSTALL_DIR}/${BINARY_NAME}"
}

check_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) log "note: $INSTALL_DIR is not on your PATH; add: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

detect_platform
latest_version
install
check_path
"${INSTALL_DIR}/${BINARY_NAME}" version \
  || { err "installed but 'unity-sync version' failed"; exit 1; }
