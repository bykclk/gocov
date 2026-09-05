---
name: pre-pr
description: Pre-PR checklist for the current branch. Default = the cheap mechanical gates only (build, vet, gofmt, tests, pins, docs build, migrations). "review" adds a low-effort /code-review; "full" adds /simplify plus a medium /code-review. Reports what is left before the PR can be opened.
argument-hint: "[review|full] [base branch, default main]"
allowed-tools: Bash, Read, Grep, Glob, Edit, Skill
---

# Pre-PR checklist

Arguments: **$ARGUMENTS**. The first word may be a tier — `review` or `full`; anything
else (or nothing) is the default tier. A remaining word is the base branch (default
`main`). Work on the current branch's diff against that base; do not switch branches.

Tiers exist because the review passes are the expensive part: they read the whole diff,
the touched files and their neighbours, and run verification subagents, while the gates
are shell commands that cost nothing.

| Tier | What runs |
|---|---|
| default | steps 1, 2, 4, 5 |
| `review` | + `/code-review low` in step 3 |
| `full` | + `/simplify`, then `/code-review medium` in step 3 |

Never run a review pass in the default tier, even if the diff looks risky; say that it
looks risky and suggest `/pre-pr review` instead.

## 1. What changed

```sh
git status --short
git diff --stat <base>...HEAD
```
Refuse to continue on `main` itself, or when the tree has files that were not part of
this session's work and the user has not said they belong to the change. List
uncommitted files: they are part of the PR only once committed.

## 2. Mechanical gates, in order, stop at the first red

```sh
go build ./... && go vet ./... && gofmt -l . && go test ./...
```
`gofmt -l` printing any path is a failure. If the test Postgres is up the store
integration tests run too; say whether they ran or skipped.

Then, only when the diff touches them:
- `docs/`, `zensical.toml`, `overrides/`: run `/docs-check`.
- `docs/gitlab-ci.md`, `docs/ci-other.md`, `docs/self-hosting.md`,
  `internal/server/templates/onboarding.html`, `internal/hosted/`: run `/check-pins`.
- `internal/config/` or `docs/configuration.md`: `go test ./internal/config/` covers
  `TestConfigurationDocIsInSync`; make sure it was not skipped.
- a new migration under `internal/store/postgres/migrations/`: confirm its number is
  the next one and that no shipped migration was edited (`git diff <base> --stat -- internal/store/postgres/migrations/`).

## 3. Review passes (`review` and `full` tiers only)

- `review`: run `/code-review low` on the diff. Fix what is real; list what you judged
  not worth changing and why.
- `full`: run `/simplify` first (reuse, simplification, efficiency; it applies its
  fixes), re-run step 2's gates if it changed anything, then `/code-review medium`.

Do not run the same pass twice in one invocation. If the user already ran
`/code-review` on this diff in this session, say so and skip it.

## 4. Docs and conventions

- User-visible behaviour changed: the matching page under `docs/` changed too, and no
  user page mentions an environment variable or deployment.
- Commit subjects are conventional (`feat:`, `fix:`, `chore:`, `refactor:`, `docs:`).
  release-please reads them: `feat`/`fix` make a release, `chore` does not. Say which
  kind this PR is.
- No commit carries an AI attribution trailer (a hook refuses them, but check
  `git log <base>..HEAD --format=%B`).

## 5. Report

One block: tier used, gates (green/red, with output for any red), review findings fixed
and deferred (if a review tier ran), docs status, release impact, and the exact remaining
steps. Do not open the PR or push: the user does that, or asks for it explicitly.
