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
# Where releases are read from. Overridable so install_test.go can run this script end to
# end against a stub rather than the live GitHub, which is the only way the platform
# mapping and the refusals below get exercised at all.
API_BASE="${UNITY_SYNC_INSTALL_API:-https://api.github.com}"
DOWNLOAD_BASE="${UNITY_SYNC_INSTALL_DOWNLOAD:-https://github.com}"

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
  VERSION=$(fetch "${API_BASE}/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || true)
  VERSION=${VERSION#v}
  [[ -n "$VERSION" ]] || { err "could not resolve the latest version from the GitHub API"; exit 1; }
  log "latest version: $VERSION"
}

# check_executable refuses an asset that is not a native binary for this platform, the way
# `unity-sync update` does before it swaps a working install. A release that shipped an
# error page or the wrong artifact would otherwise be chmod 0755'd over the binary on PATH,
# and the smoke test at the end only notices once that has already happened.
check_executable() {
  local magic; magic=$(head -c4 "$1" | od -An -tx1 | tr -d ' \n') || true
  case "$(uname -s)" in
    Linux*)
      [[ "$magic" == 7f454c46* ]] || { err "the downloaded file is not a Linux executable"; exit 1; } ;;
    Darwin*)
      case "$magic" in
        cffaedfe*|cefaedfe*|cafebabe*) ;;
        *) err "the downloaded file is not a macOS executable"; exit 1 ;;
      esac ;;
    # No silent fall-through. detect_platform refuses an OS this does not build for, so
    # reaching here means a platform was added there and not here — and the check before
    # the rename would then pass on anything at all.
    *) err "no executable signature is known for $(uname -s)"; exit 1 ;;
  esac
}

install() {
  local file="${BINARY_NAME}-${VERSION}-${PLATFORM}.zip"
  TMPDIR_SELF=$(mktemp -d)
  local tmp="$TMPDIR_SELF"

  # The browser download URL serves everyone, so there is one code path rather than a
  # token-only branch that had to find the asset's API url by grepping pretty-printed JSON.
  curl -fsSL -o "${tmp}/${file}" "${DOWNLOAD_BASE}/${REPO}/releases/download/v${VERSION}/${file}" \
    || { err "could not download ${file}"; exit 1; }

  command -v unzip >/dev/null 2>&1 || { err "unzip is required"; exit 1; }
  unzip -q "${tmp}/${file}" -d "$tmp"
  [[ -f "${tmp}/${BINARY_NAME}" ]] || { err "${file} contains no ${BINARY_NAME}"; exit 1; }
  check_executable "${tmp}/${BINARY_NAME}"
  mkdir -p "$INSTALL_DIR"

  # Staged inside the install dir so the last step is a same-filesystem rename. mktemp -d
  # lands in /tmp, usually a different filesystem, where mv degrades to copy-then-unlink
  # and an interrupted upgrade leaves a truncated binary in place of a working one.
  STAGED=$(mktemp "${INSTALL_DIR}/.${BINARY_NAME}.XXXXXX")
  cp "${tmp}/${BINARY_NAME}" "$STAGED"
  # Set, not `chmod +x`: mktemp makes the file 0600, so adding the execute bits alone
  # would install 0711 and strip read access from group and other.
  chmod 0755 "$STAGED"
  # Flushed before the rename, the way selfupdate.Replace does it. The rename is durable
  # ahead of the data it publishes, so a crash inside the writeback window leaves a
  # truncated binary on PATH — and the smoke test below has already run by then. macOS's
  # sync takes no operand, hence the fallback.
  sync "$STAGED" 2>/dev/null || sync
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
