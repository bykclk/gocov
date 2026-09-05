#!/bin/bash
# Stop: before Claude ends a turn, make sure the tree still builds, vets and
# passes its unit tests. A failure is handed back as a blocking reason so the
# turn continues with the fix instead of ending on a broken tree.
input=$(cat)
[ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false')" = "true" ] && exit 0
root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
cd "$root" || exit 0
# Only bother when Go sources changed relative to HEAD (or are untracked).
git status --porcelain -- '*.go' 'go.mod' 'go.sum' | grep -q . || exit 0
for step in "go build ./..." "go vet ./..." "go test ./..."; do
  out=$($step 2>&1) || {
    reason=$(printf '%s failed:\n%s' "$step" "$(printf '%s\n' "$out" | grep -vE '^(ok|\?)\s' | tail -25)")
    jq -n --arg r "$reason" '{decision:"block", reason:$r}'
    exit 0
  }
done
exit 0
