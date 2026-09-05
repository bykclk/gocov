---
name: verify-release
description: Check that a gocov release actually shipped — binaries, action pin and v1 tag, pipe image on both arches, pipe tag on both remotes — and say who owns each failure.
argument-hint: "[tag, e.g. v0.21.0] (empty = the newest gocov release)"
allowed-tools: Bash, Read, Grep, Glob
context: fork
---

# Verify a gocov release

Run `scripts/verify-release.sh $ARGUMENTS`. It reads the published world only
(GitHub releases and tags, the default branches, Docker Hub, GHCR, Bitbucket's tag
list), never the working tree, and it does not stop at the first failure, so read the
whole output before saying anything. The script header and `docs/development.md`
§ Releasing explain what each check guards.

Report in this order:

1. The summary line (`N passed, M failed, K skipped`) and the verdict.
2. Every `FAIL`, each with the repository that owns the fix and the next action:
   - **gocov @ tag** — missing binaries or checksums: the tag build in
     `gocov/gocov` (`gh run list --workflow release.yml`). Stale install snippets on
     main: the release commit did not bump the pins; `/check-pins` shows which file.
   - **gocov-action** — release or main pins the old CLI, or `v1` does not point at
     the new tag: the action's bump PR is unmerged (`gh pr list -R gocov/gocov-action`)
     or its release step failed.
   - **upload-pipe** — release or main bakes the old CLI: the pipe's bump PR is
     unmerged (`gh pr list -R gocov/upload-pipe`). Tag on GitHub but not Bitbucket:
     the mirror push did not run; the Atlassian catalog reads Bitbucket.
   - **docker.io / ghcr.io** — image missing an architecture or reporting an older
     version: the image build for that tag (`gh run list -R gocov/upload-pipe`, or
     `gh run list --workflow release.yml` here).
3. Every `skip`, with its reason. The two "open the image and read its version" checks
   skip without docker; the `verify-release` workflow has docker:
   `gh workflow run verify-release.yml -f tag=<TAG>`.

Timing matters: right after the gocov tag lands, the action and pipe checks are
legitimately behind until their bump PRs merge. Say "waiting on the bump PRs", not
"broken". Exit code 2 means the script itself could not run (missing tool, unreadable
release); fix that before interpreting anything.

You report; the user acts. Never merge a PR, rerun a release workflow, push a tag, or
retag `v1`.
