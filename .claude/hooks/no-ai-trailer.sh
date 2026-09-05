#!/bin/bash
# PreToolUse (Bash): refuse any shell command that would attribute a commit,
# PR or release to Claude. This repo's commits and PRs are authored by the
# maintainer alone.
cmd=$(jq -r '.tool_input.command // empty')
if printf '%s' "$cmd" | grep -qiE 'co-authored-by:[^"'"'"']*(claude|anthropic)|generated with \[?claude code|noreply@anthropic\.com'; then
  jq -n '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:"gocov commits and PRs carry no AI attribution. Remove the Co-Authored-By / Generated with Claude Code lines and run the command again."}}'
fi
exit 0
