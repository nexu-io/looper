#!/usr/bin/env bash
# Local mirror of CI's `verify` job — run this before you push and CI won't
# surprise you. Same four gates, same order as .github/workflows/ci.yml:
#   gofmt -l  →  go vet  →  go test  →  go build (with release ldflags)
#
# Usage:
#   scripts/verify.sh                 # check everything (fails like CI would)
#   scripts/verify.sh --fix           # gofmt -w first, then run the gates
#   scripts/verify.sh --install-hooks # point git at .githooks (one-time per clone)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

FIX=0
for arg in "$@"; do
  case "$arg" in
    --fix) FIX=1 ;;
    --install-hooks)
      git config core.hooksPath .githooks
      chmod +x .githooks/* 2>/dev/null || true
      echo "✓ git hooks enabled (core.hooksPath=.githooks) — pre-commit now auto-gofmts"
      exit 0 ;;
    -h|--help)
      sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

step() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

step "gofmt"
if [ "$FIX" -eq 1 ]; then
  gofmt -w .
  echo "  gofmt -w applied"
fi
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '  these files need gofmt (run: scripts/verify.sh --fix):\n%s\n' "$unformatted" >&2
  exit 1
fi
echo "  clean"

step "go vet ./..."
go vet ./...

step "go test ./..."
go test ./...

step "go build (release ldflags)"
LOOPER_BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
LOOPER_BUILD_GIT_SHA="$(git rev-parse HEAD)" \
  go build -ldflags "$(go run ./tools/go-build-flags)" ./...

printf '\n\033[32m✓ verify passed — matches CI\033[0m\n'
