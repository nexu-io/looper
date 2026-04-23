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

is_installer_owned_cli_path() {
  path="$1"
  dir="${path%/*}"
  case "$path" in
    "$HOME/.local/bin/looper"|"$HOME/.looper/bin/looper") return 0 ;;
    "$HOME"/go/bin/looper|"$HOME"/*/go/bin/looper) return 1 ;;
    /opt/homebrew/*|/usr/local/Homebrew/*) return 1 ;;
    "$HOME"/*/looper)
      if in_path_dir "$dir"; then
        return 0
      fi
      return 1
      ;;
    *) return 1 ;;
  esac
}

cli_path="${LOOPER_INSTALL_PATH:-}"
explicit_cli_path=0
if [ -n "$cli_path" ]; then
  explicit_cli_path=1
elif command -v looper >/dev/null 2>&1; then
  cli_path="$(command -v looper)"
fi

looper_home="$HOME/.looper"

if [ -n "$cli_path" ]; then
  if is_installer_owned_cli_path "$cli_path"; then
    remove_if_exists "$cli_path"
  elif [ "$explicit_cli_path" -eq 1 ] && confirm "Remove CLI binary at $cli_path? This path is not recognized as installer-owned."; then
    remove_if_exists "$cli_path"
  else
    log "Skipped CLI binary at $cli_path (not recognized as installer-owned; set LOOPER_INSTALL_PATH and confirm to remove)"
  fi
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
