#!/usr/bin/env bash
set -euo pipefail

repo="${1:-nexu-io/looper}"
out="internal/e2e/githubcontract/testdata/gh-schema/schema.json"

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

extract_fields() {
  local noun="$1"
  local args=()
  case "$noun" in
    issue-list) args=(issue list --repo "$repo") ;;
    pr-list) args=(pr list --repo "$repo") ;;
    pr-view) args=(pr view 1 --repo "$repo") ;;
    *) return 1 ;;
  esac
  if gh "${args[@]}" --json __looper_invalid_field__ >/dev/null 2>"$tmp_dir/$noun.err"; then
    echo "unexpected success while probing $noun" >&2
    return 1
  fi
  python3 - <<'PY' "$tmp_dir/$noun.err"
import re, sys, json
text = open(sys.argv[1]).read()
match = re.search(r'Available fields:\n((?:\s+.+\n?)*)', text)
if not match:
    raise SystemExit(f"unable to parse available fields from {sys.argv[1]}\n{text}")
fields = [line.strip() for line in match.group(1).splitlines() if line.strip()]
print(json.dumps(fields))
PY
}

issue_list="$(extract_fields issue-list)"
pr_list="$(extract_fields pr-list)"
pr_view="$(extract_fields pr-view)"

python3 - <<'PY' "$out" "$issue_list" "$pr_list" "$pr_view"
import json, sys, pathlib
out = pathlib.Path(sys.argv[1])
payload = {
  "jsonFieldAllowlist": {
    "issue list": json.loads(sys.argv[2]),
    "pr list": json.loads(sys.argv[3]),
    "pr view": json.loads(sys.argv[4]),
  }
}
out.parent.mkdir(parents=True, exist_ok=True)
out.write_text(json.dumps(payload, indent=2) + "\n")
print(f"wrote {out}")
PY
