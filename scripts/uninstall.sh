#!/bin/sh

set -eu

log() {
  printf '%s\n' "$*"
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

remove_if_exists() {
  path="$1"
  if [ -e "$path" ] || [ -L "$path" ]; then
    rm -rf "$path"
    log "Removed $path"
  fi
}

cli_path="${LOOPER_INSTALL_PATH:-}"
if [ -z "$cli_path" ] && command -v looper >/dev/null 2>&1; then
  cli_path="$(command -v looper)"
fi

looper_home="$HOME/.looper"

if [ -n "$cli_path" ]; then
  remove_if_exists "$cli_path"
fi

remove_if_exists "$looper_home/bin/looperd"
remove_if_exists "$looper_home/bin/looperd.prev"
remove_if_exists "$looper_home/state"
remove_if_exists "$looper_home/run/upgrade.lock"

if confirm "Also remove config, database, backups, logs, and worktrees under $looper_home?"; then
  remove_if_exists "$looper_home/config.json"
  remove_if_exists "$looper_home/looper.sqlite"
  remove_if_exists "$looper_home/backups"
  remove_if_exists "$looper_home/logs"
  remove_if_exists "$looper_home/worktrees"
fi

log "Looper uninstall complete"
