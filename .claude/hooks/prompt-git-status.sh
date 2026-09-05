#!/bin/bash
# UserPromptSubmit: put the working tree's state in front of Claude on every
# prompt, so edits made outside the session (an editor, a `go fix` sweep, a
# checkout) are seen before they are staged or built over. Silent when clean.
root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
cd "$root" || exit 0
branch=$(git symbolic-ref --short -q HEAD || git rev-parse --short HEAD)
status=$(git status --porcelain 2>/dev/null)
[ -z "$status" ] && exit 0
count=$(printf '%s\n' "$status" | wc -l | tr -d ' ')
shown=$(printf '%s\n' "$status" | head -25)
[ "$count" -gt 25 ] && shown="$shown
... and $((count - 25)) more"
jq -n --arg b "$branch" --arg n "$count" --arg s "$shown" \
  '{hookSpecificOutput:{hookEventName:"UserPromptSubmit",additionalContext:("Working tree on " + $b + ": " + $n + " changed file(s). Files you did not edit in this session were changed outside it; check before staging or building on them.\n" + $s)}}'
