# Migrating from Codecov or Coveralls

Both services and gocov measure the same thing and hand it to the same places: a status on the commit, a comment on
the pull request, a badge in the README. So a migration is mostly a translation — the status thresholds you have
become a [coverage gate](coverage-gate.md), the flags or parallel jobs become [parts](parts.md), the upload step is
swapped for gocov's — and it can be done with the old tool still running.

What changes underneath: no upload secret ([an identity token from your CI](#tokens) replaces it), the same product
on GitHub, GitLab and Bitbucket Cloud, and a self-hostable single binary if you would rather run it yourself. What
does not carry over: history. gocov starts measuring from the first upload, so run both for a while before you
switch the badge and the required checks.

## The order that works

1. [Sign in](getting-started.md), claim the workspace and connect it to the forge. Nothing changes in your repos yet.
2. Add the gocov upload step **next to** the existing one. From the next push both report; compare the numbers on a
   few pull requests. Paths that do not line up show as an empty diff coverage —
   [Why coverage changed](coverage-changed.md) has the fixes.
3. Set the gate from your old thresholds ([tables below](#codecovyml)), on the workspace's settings page as the
   default for new repos and on each repo you have already uploaded from.
4. Switch the required check in branch protection to gocov's, swap the badge, remove the old step, the old
   configuration file and the old token.

## From Codecov

### The upload step

`codecov/codecov-action` and `gocov/gocov-action` take the same shape. The differences are the inputs:

| Codecov input | gocov | Notes |
|---|---|---|
| `token` | *(none)* or `token` | Grant the job `id-token: write` and no secret is needed; a `GOCOV_TOKEN` secret is the alternative — see [tokens](#tokens). |
| `use_oidc: true` | `permissions: id-token: write` | The permission alone turns it on; there is no input. |
| `files` | `files` | Same comma-separated list; globs allowed. |
| `directory`, `disable_search` | — | gocov never searches for reports: name them in `files`. |
| `flags` | `part` | One part per upload; a Codecov upload with two flags becomes two uploads, one per part. |
| `name` | — | Uploads are listed by commit, part and time. |
| `fail_ci_if_error` (default `false`) | `fail-on-error` (default `true`) | gocov fails the step by default when the upload fails; set `false` for the Codecov behaviour. |
| `slug` | — | Detected from the CI environment. |
| `verbose` | — | The step's log already says what was uploaded and what the server answered. |

Before and after, for a repository whose tests write `coverage.xml`:

```yaml
# before
- uses: codecov/codecov-action@v5
  with:
    token: ${{ secrets.CODECOV_TOKEN }}
    files: coverage.xml
    flags: unit
    fail_ci_if_error: true
```

```yaml
# after
permissions:
  contents: read
  id-token: write
steps:
  - uses: gocov/gocov-action@v1
    with:
      files: coverage.xml
      part: unit
```

The format (Cobertura here) is detected from the file's content; every format Codecov accepts from a mainstream test
runner is listed in [Languages & formats](languages.md). GitLab CI and Bitbucket Pipelines users swap the Codecov CLI
call for the [gocov component](gitlab-ci.md) or the [gocov pipe](bitbucket-pipelines.md) in the same way.

### codecov.yml

gocov has no configuration file; the settings below live on the workspace and repository settings pages, and a few are
CLI or action inputs. The table maps each `codecov.yml` key to where it went.

| `codecov.yml` | gocov |
|---|---|
| `coverage.status.project.default.target: 80%` | Gate → **Min coverage** `80`. |
| `coverage.status.project.default.target: auto` + `threshold: 2` | Gate → **Max coverage drop** `2`. Codecov compares with the base commit; gocov compares with the latest gate-passing upload on the default branch, so a drop cannot be laundered by re-running CI or ratcheted down push by push. |
| `coverage.status.patch.default.target: 80%` | Gate → **Min diff coverage** `80`. |
| `coverage.status.patch.default.target: auto` | No direct equivalent. Set **Min diff coverage** to the total you expect of new code, or leave it empty: diff coverage is still reported on every PR. |
| `informational: true` | Leave the corresponding gate field empty. The number is still reported; it just cannot fail the status. |
| `threshold` on `patch` | — (diff coverage has a minimum, not a drop tolerance). |
| `if_ci_failed`, `only_pulls`, `branches`, `paths` on a status | — . The gate applies to every upload; the diff rule only has something to measure on PR uploads. |
| `flags`, `flag_management` | [Parts](parts.md): `part: <name>` on each upload. Parts merge as they arrive; there is no carryforward, so a commit's report is the parts it received. |
| `ignore` | Repository settings → **Ignored files**, or the `ignore` input on the upload — [ignoring files](ignoring-files.md). Same `.gitignore`-style globs. |
| `fixes` | The CLI's `-path-prefix`; Go modules are detected from `go.mod`. See [Why coverage changed](coverage-changed.md). |
| `comment.layout`, `comment.behavior` | The [PR comment](pull-requests.md) has one layout — total, delta, diff coverage, the uncovered changed lines — and always updates in place. |
| `comment.require_changes` | — (the comment is posted on every PR upload). |
| `github_checks.annotations` | On by default: the check run annotates uncovered changed lines once the workspace is [connected](connecting.md). |
| `codecov.require_ci_to_pass`, `notify.after_n_builds` | — . gocov reports as soon as an upload lands; see the note on parts and the gate below. |
| `coverage.range`, `coverage.precision`, `coverage.round` | — . The badge is red below 50%, yellow to 75%, green above; percentages carry one decimal. |
| `codecov.max_report_age` | — (uploads are accepted for any commit gocov can see). |

One behavioural difference to plan for: with several parts, Codecov waits for `after_n_builds` before deciding;
gocov evaluates the gate against the merged report **as parts arrive**, so it can fail transiently until the last
part lands and then correct itself. Sequence the required check after all coverage jobs, as [parts](parts.md)
describes.

### Required checks and the badge

In branch protection, replace `codecov/project` and `codecov/patch` with `gocov` (the commit status) or
`gocov coverage` (the check run) — one status carries both rules. Then the badge:

```markdown
<!-- before -->
[![codecov](https://codecov.io/gh/myorg/myrepo/branch/main/graph/badge.svg?token=…)](https://codecov.io/gh/myorg/myrepo)
<!-- after -->
[![coverage](https://app.gocov.dev/badge/github/myorg/myrepo.svg)](https://app.gocov.dev/repos/github/myorg/myrepo?ref=badge)
```

Finally: delete `codecov.yml`, the `CODECOV_TOKEN` secret, and uninstall the Codecov app from the organization.

## From Coveralls

### The upload step

| Coveralls input | gocov | Notes |
|---|---|---|
| `github-token` | *(none)* | gocov never needs a GitHub token in the job: identity comes from `id-token: write` (or a `GOCOV_TOKEN` secret), and statuses are posted through the workspace's connection. |
| `file`, `files` (space-separated) | `files` (comma-separated) | Globs allowed. |
| `format` | — | Detected from the file's content. |
| `flag-name` | `part` | |
| `parallel: true`, `parallel-finished: true` | — | Parts merge as they arrive; there is no finish job. |
| `carryforward` | — | No carryforward: a commit's report is the parts it received. |
| `base-path` | — | Report paths have to match the repository's; see [Why coverage changed](coverage-changed.md). |
| `fail-on-error` (default `true`) | `fail-on-error` (default `true`) | Same. |
| `allow-empty` | — | An empty report is an error. |
| `compare-ref` | — | A PR is compared against its base through the forge's diff; the default branch against the previous upload. |
| `coveralls-endpoint` | `server` | Only when self-hosting gocov. |

A matrix with a finish job collapses to the matrix alone:

```yaml
# before
test:
  strategy: { matrix: { os: [ubuntu, macos] } }
  steps:
    - uses: coverallsapp/github-action@v2
      with:
        parallel: true
        flag-name: ${{ matrix.os }}
finish:
  needs: test
  steps:
    - uses: coverallsapp/github-action@v2
      with:
        parallel-finished: true
        carryforward: "ubuntu,macos"
```

```yaml
# after
permissions:
  contents: read
  id-token: write
test:
  strategy: { matrix: { os: [ubuntu, macos] } }
  steps:
    - uses: gocov/gocov-action@v1
      with:
        files: coverage.out
        part: ${{ matrix.os }}
```

### Thresholds, checks and the badge

Coveralls' repository settings map onto the [gate](coverage-gate.md): **Coverage threshold for failure** is
**Min coverage**, **Coverage decrease threshold for failure** is **Max coverage drop**. Coveralls has no
per-PR-lines rule; **Min diff coverage** is the one you gain.

Replace the `coverage/coveralls` required check with `gocov` or `gocov coverage`, then the badge:

```markdown
<!-- before -->
[![Coverage Status](https://coveralls.io/repos/github/myorg/myrepo/badge.svg?branch=main)](https://coveralls.io/github/myorg/myrepo?branch=main)
<!-- after -->
[![coverage](https://app.gocov.dev/badge/github/myorg/myrepo.svg)](https://app.gocov.dev/repos/github/myorg/myrepo?ref=badge)
```

Finally: delete `.coveralls.yml` if you have one, the `COVERALLS_REPO_TOKEN` secret, and the Coveralls webhook or app.

## Tokens

Neither the Codecov token nor the Coveralls repo token has an equivalent that you paste. On GitHub Actions, Bitbucket
Pipelines and GitLab CI the job proves its own identity with a short-lived OIDC token the forge mints, and gocov
verifies it through the workspace's connection — nothing to create, share or rotate. The per-CI pages show the one
line that turns it on. A CI that cannot mint one uses the workspace's **upload token** as a `GOCOV_TOKEN` variable
instead; owners find it on the setup page.

## What you will not find

- **History import.** gocov starts at the first upload. The delta on the first PR compares with the first
  default-branch upload it has, so let the default branch upload once before you judge the numbers.
- **Carryforward.** A commit's merged report is the parts that were uploaded for it.
- **Per-status branch, path and flag filters.** One gate per repository, evaluated on the whole report.
- **A configuration file.** Gate, ignore patterns and connections are settings on the workspace and repository
  pages, visible to the whole team; per-upload choices are inputs on the upload step.

Self-hosting changes none of the above — set `server` on the upload step (or `GOCOV_SERVER` in CI) to your instance;
[Self-hosting](self-hosting.md) covers the rest.
