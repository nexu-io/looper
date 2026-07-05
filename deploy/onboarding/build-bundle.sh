#!/usr/bin/env bash
# Build the looper HITL onboarding bundle to hand a teammate.
#
# The bundle contains the setup prompt, the config template, the guide, and the
# team's SHARED filled env (so the teammate has everything). Distribute the zip
# PRIVATELY — with a filled env it holds secrets.
#
# Usage:
#   deploy/onboarding/build-bundle.sh /path/to/your/filled/hitl.env [out.zip]
#   deploy/onboarding/build-bundle.sh            # no env → bundles the template only
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FILLED_ENV="${1:-}"
OUT="${2:-looper-hitl-onboarding.zip}"
case "$OUT" in /*) ;; *) OUT="$PWD/$OUT" ;; esac

STAGE_PARENT="$(mktemp -d)"
STAGE="$STAGE_PARENT/looper-hitl-onboarding"
mkdir -p "$STAGE"

cp "$REPO_ROOT/deploy/onboarding/SETUP-PROMPT.md" "$STAGE/"
cp "$REPO_ROOT/deploy/config.hitl.example.json"   "$STAGE/"
cp "$REPO_ROOT/docs/GUIDE-hitl-setup.md"          "$STAGE/"

if [ -n "$FILLED_ENV" ] && [ -f "$FILLED_ENV" ]; then
  cp "$FILLED_ENV" "$STAGE/hitl.env"
  echo "• bundled filled hitl.env — this zip HAS SECRETS, distribute privately"
else
  cp "$REPO_ROOT/deploy/hitl.env.example" "$STAGE/hitl.env.example"
  echo "• no filled hitl.env given → bundled the .example template only"
  echo "  the teammate still needs the real shared secrets separately"
fi

rm -f "$OUT"
( cd "$STAGE_PARENT" && zip -r -q "$OUT" "looper-hitl-onboarding" )
rm -rf "$STAGE_PARENT"
echo "• built: $OUT"
echo "  contents:"; unzip -Z1 "$OUT" | sed 's/^/    /'
