#!/bin/bash
# PostToolUse (Edit|Write): after a .go file changes, run gofmt -l on it and
# go vet on its package. Any finding goes to stderr with exit 2 so Claude sees
# it immediately instead of at the next go test run.
f=$(jq -r '.tool_input.file_path // .tool_response.filePath // empty')
case "$f" in *.go) ;; *) exit 0 ;; esac
[ -f "$f" ] || exit 0
dir=$(dirname "$f")
problems=""
unformatted=$(gofmt -l "$f" 2>&1)
[ -n "$unformatted" ] && problems="gofmt: $f is not gofmt-formatted; run: gofmt -w $f"
vet=$(cd "$dir" && go vet . 2>&1)
if [ $? -ne 0 ]; then
  problems="${problems:+$problems
}go vet ($dir):
$vet"
fi
if [ -n "$problems" ]; then
  printf '%s\n' "$problems" >&2
  exit 2
fi
exit 0
