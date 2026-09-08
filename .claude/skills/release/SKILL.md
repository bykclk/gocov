---
name: release
description: Cut a gocov release end to end — Release-As PR, release-please PR, tag, wrapper bumps, deploy, verify. Run only when the maintainer asks for a release.
argument-hint: "[version, e.g. 0.15.0]"
allowed-tools: Bash, Read, Grep, Glob
disable-model-invocation: true
---

# Release gocov

Requested version: **$ARGUMENTS** (empty = propose one from the commits since the last tag and ask).

You are the guide here. You run the checks and the `gh` plumbing; the user does every
merge and every approval click. Never merge a PR, never approve a deploy, never push a
tag by hand unless the user explicitly says so in this session.

Background: `docs/development.md` § Releasing is the written source; this command is the
operational version of it. Read it if anything below disagrees with the repo.

## The shape of a release

One release lands in four repositories and is, in the end, **four PR merges plus two
approval clicks** (the release PR's CI run, and the production deploy):

1. `gocov` — a `Release-As:` PR states the version; release-please opens the real release
   PR; merging it tags `vX.Y.Z` and the tag build publishes binaries, the GHCR image, and
   the three wrapper bump PRs, then waits for the production deploy approval.
2. `gocov-action` — merge its bump PR; that *is* its release (tags next minor, moves `v1`).
3. `upload-pipe` — merge its bump PR; that *is* its release (Docker Hub multi-arch + the
   Bitbucket mirror push).
4. `gitlab-component` — merge its bump PR; that *is* its release (tags next minor, GitHub
   release, mirror push to gitlab.com whose pipeline publishes the CI/CD Catalog release).

Version numbers are **stated, never inferred** — commit subjects here are English
sentences, not `feat:`/`fix:`, so release-please only does PR/CHANGELOG/pin work.

## Step 0 — Preflight

Run and report, stop on anything red:

```sh
git -C . status --short          # must be clean; uncommitted work is a decision, not a detail
git fetch origin && git log --oneline origin/main..main && git log --oneline main..origin/main
git log --oneline "$(git describe --tags --abbrev=0)"..origin/main   # what is shipping
go build ./... && go vet ./... && go test ./...
scripts/check-pins.sh
gh run list --branch main --limit 3                                  # main's CI is green
```

If the working tree is dirty: ask whether that work lands in this release (then it needs
its own PR merged to main first) or waits for the next one (stash/branch it). Do not
release around a dirty tree silently. Landing it is cheap and usually the right answer —
branch from main, commit it with a real subject, open the PR, wait for its checks, merge,
and only then cut the `Release-As:` PR. Do that *before* step 1 so the release PR does not
have to be reopened.

Then agree on the version with the user. Rule of thumb while pre-1.0: user-visible
features → minor (`0.14.0` → `0.15.0`); fixes/doc-only → patch. Say what the commits since
the last tag contain and propose a number.

## Step 1 — Ask for the version (a PR, main takes nothing else)

**Check first whether release-please already has the PR open.** Any commit since the
last tag with a conventional subject (`feat:`, `fix:`, …) makes release-please infer the
version itself and open `chore(main): release X.Y.Z` on the first such merge, refreshing
it on every push to main. If that PR exists and states the version you agreed on, skip
this step and go to Step 2 — a `Release-As:` PR would only add an empty commit (v0.18.0:
#110 was opened and closed unmerged for exactly this reason).

```sh
gh pr list --search "chore(main): release in:title" --json number,title,updatedAt
```

Only when no such PR exists, or it names the wrong version:

```sh
git checkout -b release-<VERSION>
git commit --allow-empty -m "chore: release <VERSION>" -m "Release-As: <VERSION>"
git push -u origin release-<VERSION>
gh pr create --title "chore: release <VERSION>" --body "Release-As: <VERSION>"
```

**Critical:** the `Release-As:` footer must survive the squash-merge message, or
release-please sees nothing. Tell the user to check the squash body before merging.

User merges. Then `git checkout main && git pull`.

## Step 2 — The release PR

release-please runs on the push to main and opens `chore(main): release <VERSION>` holding
the version, the CHANGELOG entry, and the bumped pins (gitlab-ci.md, ci-other.md,
self-hosting.md, onboarding.html, internal/hosted). Watch for it:

```sh
gh run list --workflow release-please.yml --limit 3
gh pr list --search "release" --limit 5
gh pr diff <N>          # review the pins and the CHANGELOG with the user
```

The PR's own `ci.yml` run sits behind **"Approve and run"** on the PR page — the bot never
graduates out of `first_time_contributors`, so it asks every release, and the ruleset's
required checks block the merge until that run has been approved and is green (`gh pr checks
<N>` shows them pending until then). That click is the user's; point it out before the merge.

Until this is merged nothing is tagged — a wrong version is a PR comment, not a burnt tag.
User merges it.

## Step 3 — The tag build, and the deploy approval

Merging tags `v<VERSION>` and, because a GITHUB_TOKEN tag cannot trigger a workflow,
release-please *calls* `release.yml` directly. The push to main is the user's merge, so no
"Approve and run" here. Watch it:

```sh
gh run list --limit 5
gh run watch <run-id>
```

One human gate in this run, the user's click:

- **production deploy** — the `production` environment's required reviewer. Approve when
  the image job is done; the deploy pulls the image, rolls app.gocov.dev, and smoke-tests
  `/healthz` plus a real upload from `gocov/smoke`.

The build publishes: 10 binaries + `checksums.txt` on the release, the GHCR server image
(`vX.Y.Z`, `X.Y`, `latest`), and the three wrapper bump PRs.

## When the release contains a server-side feature the wrappers use

Two ordering rules, both learned on v0.16.0 (OIDC tokenless uploads):

- The wrappers' own feature PRs (the ones teaching them the new flow) go onto their mains
  **before** the gocov release. They carry no `release` label, so merging them publishes
  nothing; the bump PR this release opens then tags a wrapper release containing both the
  feature and the new CLI pin, and no wrapper version ever ships the feature without it.
- Merge those bump PRs **after the production deploy is green**, not before. A wrapper that
  speaks the new protocol against a server still on the old version fails for real users;
  the deploy is what makes the feature exist.

Neither applies to an ordinary release — there, the bump PRs can be merged as soon as they
appear.

## Step 4 — The wrappers

```sh
gh pr list --repo gocov/gocov-action     --label release
gh pr list --repo gocov/upload-pipe      --label release
gh pr list --repo gocov/gitlab-component --label release
```

Merging each one **is** that wrapper's release. Merge the action's first (its `v1` is what
most users track), then the pipe's, then the component's. If a bump PR never appeared, the
App token step in `release.yml` failed — read that job's log before opening anything by
hand (the App must be installed on all three wrapper repos).

## Step 5 — Verify

```sh
scripts/verify-release.sh v<VERSION>
```

23/23 is the expected result once the wrappers are merged and their builds are done (the
pipe's multi-arch image and the component's GitLab pipeline take a few minutes). Before
that, the wrapper checks are legitimately behind — that is why the `verify-release`
workflow reports rather than fails on `release: published`.

Give the user a final summary: the version, the four merged PRs, the deploy result, and
the verify score.

## Gotchas worth remembering

- The release build's jobs live **under the release-please run** (`publish / release`,
  `publish / image`, `publish / bump-wrappers`, `publish / selfhost-smoke`,
  `publish / deploy / deploy`), because release-please *calls* `release.yml`.
  `gh run list --workflow release.yml` shows no new run — read
  `gh api repos/gocov/gocov/actions/runs/<release-please-run-id>/jobs` and
  `.../pending_deployments` instead. A run in status `waiting` is the deploy gate.
- upload-pipe publishes plain git tags, not GitHub releases: `gh release list` there is
  always empty; use `gh api repos/gocov/upload-pipe/tags`.

- A tag pushed with GITHUB_TOKEN starts no workflow; that is why `release.yml` is called,
  not triggered. Never "fix" it by re-pushing the tag.
- `-X main.version` comes from `$TAG`, not `GITHUB_REF_NAME` (on the called path that
  would be `main`).
- The production box follows *release tags*, not main: the deploy checks out the tag in
  `/opt/gocov`, so compose and Caddyfile move with the release.
- The pipe's Bitbucket mirror needs `BITBUCKET_MIRROR_APP_PASSWORD`; git auth there uses
  the fixed username `x-bitbucket-api-token-auth`. It warns rather than fails when unset —
  verify-release is what notices.
- Escape hatch: if the bot is in the way, a hand-pushed `v*` tag still runs the whole
  release build, creating the GitHub release itself.
- Rollback: dispatch `deploy.yml` with an older release tag. No rebuild, seconds.
- `gh run watch` returns immediately in this environment instead of blocking. To wait on a
  run, poll `gh api repos/gocov/gocov/actions/runs/<id> --jq .status` in a background Bash
  loop until it reads `waiting` (the deploy gate) or `completed`.
- `verify-release.sh` skips its two "open the image and read its version" checks when
  Docker is not running locally — that is 21/0/2, not a failure. For a true 23/23, dispatch
  the workflow instead: `gh workflow run verify-release.yml -f tag=v<VERSION>`.
