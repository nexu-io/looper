#!/bin/sh

set -eu

OWNER="${LOOPER_GITHUB_OWNER:-powerformer}"
REPO="${LOOPER_GITHUB_REPO:-looper}"
VERSION="${LOOPER_VERSION:-latest}"

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

detect_target() {
  os="$(uname -s)"
  arch="$(uname -m)"

  [ "$os" = "Darwin" ] || fail "unsupported platform: $os (supported: macOS)"

  case "$arch" in
    arm64|aarch64) printf 'darwin-arm64\n' ;;
    *) fail "unsupported architecture: $arch (supported: arm64)" ;;
  esac
}

in_path_dir() {
  candidate="$1"
  old_ifs=$IFS
  IFS=:
  for entry in $PATH; do
    [ "$entry" = "$candidate" ] && IFS=$old_ifs && return 0
  done
  IFS=$old_ifs
  return 1
}

guess_profile() {
  shell_name="${SHELL:-}"
  shell_name="${shell_name##*/}"
  case "$shell_name" in
    zsh) printf '%s/.zprofile\n' "$HOME" ;;
    bash) printf '%s/.bash_profile\n' "$HOME" ;;
    *) printf '%s/.profile\n' "$HOME" ;;
  esac
}

confirm() {
  prompt="$1"
  if [ ! -t 0 ]; then
    return 1
  fi
  printf '%s [y/N] ' "$prompt" >&2
  read -r answer || return 1
  case "$answer" in
    y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

append_path_export() {
  profile="$1"
  install_dir="$2"
  if [ "$install_dir" = "$HOME/.local/bin" ]; then
    export_line='export PATH="$HOME/.local/bin:$PATH"'
  else
    export_line="export PATH=\"$install_dir:\$PATH\""
  fi
  mkdir -p "$(dirname "$profile")"
  touch "$profile"
  if grep -F "$export_line" "$profile" >/dev/null 2>&1; then
    return 0
  fi
  {
    printf '\n# Added by looper installer\n'
    printf '%s\n' "$export_line"
  } >>"$profile"
}

pick_install_dir() {
  if [ -n "${LOOPER_INSTALL_DIR:-}" ]; then
    printf '%s\n' "$LOOPER_INSTALL_DIR"
    return 0
  fi

  printf '%s/.local/bin\n' "$HOME"
}

build_download_base() {
  tag="$1"
  if [ "$tag" = "latest" ]; then
    printf 'https://github.com/%s/%s/releases/latest/download\n' "$OWNER" "$REPO"
  else
    printf 'https://github.com/%s/%s/releases/download/%s\n' "$OWNER" "$REPO" "$tag"
  fi
}

verify_checksum() {
  file="$1"
  expected="$2"
  if command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$file" | awk '{print $1}')"
  elif command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$file" | awk '{print $1}')"
  elif command -v openssl >/dev/null 2>&1; then
    actual="$(openssl dgst -sha256 "$file" | awk '{print $NF}')"
  else
    fail "missing checksum tool (need shasum, sha256sum, or openssl)"
  fi

  [ "$actual" = "$expected" ] || fail "checksum mismatch: expected $expected, got $actual"
}

curl_supports_progress_bar() {
  curl --help all 2>/dev/null | grep -q -- '--progress-bar'
}

show_download_progress() {
  case "${LOOPER_DOWNLOAD_PROGRESS:-auto}" in
    0|false|False|FALSE|no|No|NO|never|Never|NEVER)
      return 1
      ;;
    1|true|True|TRUE|yes|Yes|YES|always|Always|ALWAYS)
      curl_supports_progress_bar
      return
      ;;
  esac

  [ -t 2 ] && curl_supports_progress_bar
}

download_file() {
  url="$1"
  output="$2"

  if show_download_progress; then
    curl -fL --progress-bar "$url" -o "$output"
  else
    curl -fsSL "$url" -o "$output"
  fi
}

need_cmd curl
need_cmd grep

target="$(detect_target)"
asset="looper-$target"
download_base="$(build_download_base "$VERSION")"
binary_url="$download_base/$asset"
checksum_url="$download_base/$asset.sha256"

install_dir="$(pick_install_dir)"
mkdir -p "$install_dir"

profile_updated=0
if ! in_path_dir "$install_dir"; then
  if [ "$install_dir" = "$HOME/.local/bin" ]; then
    profile="$(guess_profile)"
    if confirm "~/.local/bin is not on PATH. Add it to $(basename "$profile")?"; then
      append_path_export "$profile" "$install_dir"
      profile_updated=1
    fi
  fi
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM HUP

tmp_binary="$tmp_dir/looper"
tmp_checksum="$tmp_dir/$asset.sha256"

log "Downloading $binary_url"
download_file "$binary_url" "$tmp_binary"
download_file "$checksum_url" "$tmp_checksum"

expected_checksum="$(awk '{print $1}' "$tmp_checksum")"
[ -n "$expected_checksum" ] || fail "invalid checksum file: $checksum_url"

verify_checksum "$tmp_binary" "$expected_checksum"

chmod 0755 "$tmp_binary"
install_path="$install_dir/looper"
mv "$tmp_binary" "$install_path"

log "Installed looper to $install_path"
if [ "$profile_updated" -eq 1 ]; then
  log "Added $install_dir to PATH in $(guess_profile)"
fi

if ! in_path_dir "$install_dir"; then
  log "Open a new shell or run: export PATH=\"$install_dir:\$PATH\""
fi

log ""
log "This installer only installs the looper CLI."
log "looper bootstrap will install/start the matching looperd daemon."
log ""
log "Next steps:"
log "  looper bootstrap"
log "  looper status"
log ""
log "Manual daemon fallback/debug commands:"
log "  looper daemon install"
log "  looper daemon start"
