---
name: check-pins
description: Check that every copyable install snippet (CI recipe docs and the onboarding wizard) pins the same CLI release, and fix the stragglers.
allowed-tools: Bash, Read, Grep, Glob, Edit
---

# Check the CLI version pins

Every install snippet gocov hands a user names an exact CLI release, copied into
several places because some are Markdown and one is a Go template. Run
`scripts/check-pins.sh`. It holds those copies to *each other*, nothing more: whether
the version is the newest release is `/verify-release`'s job, after a release.

- Pass: one line, `check-pins: N pins, all at vX.Y.Z`. Say so and stop.
- Fail: the script lists every pin as `file:line: text` and the set of versions found.

On failure, pick the intended version first. Normally one file was missed by the
release bump and the newest version in the set is right; if the split is even or the
newest version has no matching `gocov/gocov` release tag, ask before editing. Then
fix the stragglers and re-run until green. What to know while editing:

- The pinned snippets in `docs/gitlab-ci.md`, `docs/ci-other.md` and `docs/self-hosting.md`
  sit between `x-release-please-start-version` / `x-release-please-end` markers, and
  release-please rewrites them on the next release. Change the version inside the
  markers only; never move or remove the markers.
- The wizard copy is `internal/server/templates/onboarding.html`; `internal/hosted`
  holds the version the server itself advertises, and `TestPinnedCLIVersionIsInSync`
  there fails when the docs and that constant disagree. After editing, run:

  ```sh
  scripts/check-pins.sh && go test ./internal/hosted/
  ```

Report which files were behind and what they now say. The commit is the user's.
