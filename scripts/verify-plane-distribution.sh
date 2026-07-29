#!/bin/sh

set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT INT TERM HUP

cd "$repo_root"

printf '%s\n' '==> Running Plane connection and configuration tests'
go test ./internal/cliapp ./internal/config ./internal/planestrict

printf '%s\n' '==> Building a clean Looper binary'
go build -o "$tmp_root/looper" ./cmd/looper

printf '%s\n' '==> Checking first-run diagnostics in an isolated HOME'
mkdir -p "$tmp_root/home" "$tmp_root/work"
if HOME="$tmp_root/home" "$tmp_root/looper" plane doctor --config "$tmp_root/home/missing.yaml" >"$tmp_root/doctor.out" 2>&1; then
  printf '%s\n' 'error: doctor unexpectedly passed without a configuration' >&2
  exit 1
fi
grep -q 'Plane readiness checks failed' "$tmp_root/doctor.out"
grep -q 'looper bootstrap' "$tmp_root/doctor.out"

printf '%s\n' 'Plane distribution preflight passed.'
