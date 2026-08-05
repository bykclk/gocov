# gocov

Self-hostable coverage tracking — an open-source Coveralls/Codecov
alternative. Single binary + Postgres. Bitbucket Cloud is the first
supported forge. Supported formats: Go cover profiles, LCOV tracefiles
(JavaScript/TypeScript — Jest, Vitest, nyc, c8), JaCoCo XML
(Java/Kotlin — Maven, Gradle, Android) and Cobertura XML
(Python — coverage.py/pytest-cov; also PHPUnit, coverlet, gcovr); the
format is detected from the uploaded content.

## Features (MVP)

- Parses Go cover profiles (`go test -coverprofile`), LCOV tracefiles
  (Jest/Vitest/nyc `lcov.info`), JaCoCo XML (`jacoco.xml`) and Cobertura
  XML (`coverage.xml`) into total and per-file coverage
- `POST /api/v1/upload` API with per-repo Bearer tokens
- SVG coverage badge per repo (`/badge/{workspace}/{repo}.svg`)
- Web UI: repo list → upload list → per-file coverage table
- Uploader CLI that auto-detects Bitbucket Pipelines environment
  variables and falls back to git
- Pushes a `coverage: X% (±Y%)` build status to Bitbucket commits when
  the repo has an app password configured
- Coverage gate: per-repo minimums for total and diff coverage plus a
  drop tolerance; violations push a FAILED build status, so a Bitbucket
  merge check can block the PR
- Source view: any file in an upload renders line by line with coverage
  overlay and hit counts, fetched from the forge at the exact commit and
  cached immutably (misses are cached too); without forge credentials
  the page falls back to an uncovered-line summary. When an upload has
  no `path_prefix`, recorded paths that carry an unmapped leading
  prefix (a Go module path, a CI checkout directory) are resolved by
  probing trimmed variants against the forge
- Web UI sign-in with Bitbucket: configure an OAuth consumer and every
  page requires login, allowed only for members of the workspaces the
  instance tracks (see "Enable Bitbucket sign-in"). Uploads, badges and
  health checks are unaffected; no passwords are ever stored
- Diff coverage for pull requests: fetches the PR diff from Bitbucket,
  intersects changed lines with coverage blocks, and posts a PR comment
  listing uncovered changed lines — repeated uploads update the same
  comment instead of stacking new ones
- Coverage inside the PR, via Bitbucket Code Insights: every upload
  attaches a report card to its commit (total coverage, delta, diff
  coverage, gate verdict) that Bitbucket shows in the pull request's
  Reports panel, and PR uploads annotate uncovered changed lines right
  in the diff view — reviewers see untested code exactly where they are
  reviewing it, with no plugin on the Bitbucket side. Changed files with
  no coverage data at all get a file-level marker, and the report card
  lists the worst-covered changed files while the field budget allows.
  Re-uploads replace the report and annotations in place; no coverage
  product on Bitbucket Cloud ships this today

  _[screenshot: coverage report card and inline annotations in a
  Bitbucket PR]_

- Coverage trend chart: the repo page graphs total coverage over the
  branch's recent uploads (gate failures marked in red, every point
  links to its upload) — rendered as inline SVG on the server, no
  JavaScript chart library

  _[screenshot: coverage trend chart on a repo page]_

The architecture is deliberately extensible: coverage formats sit behind
`profile.Parser`, forges behind `forge.Forge`, raw profile storage behind
`blobstore.Store`, and the database schema stores a format-agnostic
normalized model — so lcov/cobertura, GitHub/GitLab, diff coverage, and
S3 storage can be added without rewrites.

## Quick start

```sh
docker compose up
```

This starts Postgres and the server on http://localhost:8080 (migrations
apply automatically).

### Onboarding a whole workspace (recommended)

For many repos, register the workspace once and use a single token:

```sh
docker compose exec server gocov-server workspace add \
  -prefix myworkspace -default-branch main
```

Set the printed token as a *Bitbucket workspace variable* (`GOCOV_TOKEN`,
secured) together with `GOCOV_SERVER` — every repo inherits them. Repos
register themselves on their first upload; their default branch is asked
from Bitbucket when a global bot account is configured (see
Configuration), falling back to the workspace's `-default-branch`.

### Registering repos one by one

```sh
docker compose exec server gocov-server repo add \
  -slug myworkspace/myrepo \
  -default-branch main \
  -bb-username myuser -bb-app-password "$APP_PASSWORD"   # optional, for build statuses
```

Manage repos later with:

```sh
gocov-server repo list                                   # slugs, branches, credential status
gocov-server repo rotate-token -slug myworkspace/myrepo  # invalidates the old token
gocov-server repo update -slug myworkspace/myrepo \
  -default-branch develop                                # and/or -bb-username/-bb-app-password,
                                                         # or -clear-credentials
gocov-server repo remove -slug myworkspace/myrepo -force # deletes uploads and raw profiles too;
                                                         # without -force only prints a summary
gocov-server workspace list|rotate-token|update|remove   # workspace token management
```

### Enable Bitbucket sign-in

Out of the box the web UI is open and shows a banner saying so — nothing
changes on upgrade until you opt in. To require sign-in:

1. In Bitbucket, create an OAuth consumer under **Workspace settings →
   OAuth consumers → Add consumer** with
   - **Callback URL**: `https://your-gocov-host/oauth/bitbucket/callback`
     (must be exactly `GOCOV_BASE_URL` + `/oauth/bitbucket/callback`)
   - **Permissions**: *Account: Read* and *Email* only — nothing broader
     is needed
2. Set the consumer's key and secret on the server:

```sh
GOCOV_OAUTH_BITBUCKET_KEY=...
GOCOV_OAUTH_BITBUCKET_SECRET=...
```

From then on every UI page requires signing in with a Bitbucket account.
Access is decided at login time by workspace membership: by default,
members of any workspace the instance tracks (registered workspaces and
the workspace part of registered repo slugs) may sign in, and everyone
else gets a clear denial page. Set `GOCOV_ALLOWED_WORKSPACES` (comma-
separated workspace slugs) to replace the derived set with an explicit
list. Accounts are provisioned on first successful sign-in — there is
no user bookkeeping, and gocov never sees or stores passwords (the
Bitbucket tokens are discarded right after login).

CI is unaffected either way: the upload API keeps its Bearer tokens,
badges stay embeddable, `/healthz` stays open.

```sh
gocov-server user list                          # who has signed in
gocov-server user remove -email jane@example.com  # revoke immediately
```

Removal deletes the account and its sessions; the person can sign in
again (and is re-provisioned) as long as they are still a workspace
member. Sessions last 30 days; membership is re-checked at each login,
not per request.

### Coverage gate

```sh
gocov-server repo update -slug myworkspace/myrepo \
  -min-coverage 80 -min-diff-coverage 70 -max-drop 0.5
```

Each rule is optional: `-min-coverage` is the minimum total percentage,
`-min-diff-coverage` applies to the changed lines of PR uploads (skipped
when no diff coverage is available), and `-max-drop` bounds how far
total coverage may fall below the latest gate-passing upload on the
default branch (0 forbids any drop). Gate-failing uploads are recorded
but never serve as a baseline, so re-running CI cannot launder a failure
and a PR cannot ratchet coverage down push by push. Violations mark the
pushed build status FAILED — require the `gocov` build in Bitbucket's
merge checks to block such PRs — and are reported in the PR comment and
the upload response (`gate` field). `-clear-gate` removes all rules. The
same flags on `workspace add` and `workspace update` set defaults
inherited by auto-registered repos. The CLI exits non-zero on a failed
gate when run with `-fail-on-gate`.

## Uploading coverage from CI

In Bitbucket Pipelines (commit, branch, repo and PR id are auto-detected):

```yaml
- step:
    script:
      - go test ./... -covermode=atomic -coverprofile=coverage.out
      - go run github.com/bykclk/gocov/cmd/gocov@latest upload coverage.out
```

with `GOCOV_SERVER` and `GOCOV_TOKEN` set as repository variables.

On runners without a Go toolchain, use the prebuilt binaries from
[GitHub Releases](https://github.com/bykclk/gocov/releases) instead
(linux/darwin/windows, amd64 + arm64, checksums included). Pin a version
and cache the download on self-hosted runners:

```sh
ver=v0.1.0
arch=$(uname -m); case "$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac
bin="$HOME/.cache/gocov/gocov-$ver-linux-$arch"
if [ ! -x "$bin" ]; then
  mkdir -p "$(dirname "$bin")"
  curl -fsSL "https://github.com/bykclk/gocov/releases/download/$ver/gocov-linux-$arch" -o "$bin"
  chmod +x "$bin"
fi
"$bin" upload coverage.out
```
Outside CI, values fall back to git or can be passed explicitly:

```sh
gocov upload -server https://gocov.example -token $TOKEN \
  -repo myworkspace/myrepo -commit $(git rev-parse HEAD) -branch main \
  coverage.out
```

Other ecosystems upload their reports the same way — the format is
detected from the content, no flag needed:

```sh
npx jest --coverage             # or vitest run --coverage, nyc, c8 ...
gocov upload coverage/lcov.info

mvn verify                      # with the jacoco-maven-plugin
gocov upload target/site/jacoco/jacoco.xml

gradle test jacocoTestReport    # xml.required = true
gocov upload build/reports/jacoco/test/jacocoTestReport.xml

pytest --cov --cov-report=xml   # coverage.py / pytest-cov
gocov upload coverage.xml
```

JaCoCo paths are package-qualified (`com/example/Foo.java`); diff
coverage matches them against repo paths by suffix, so source roots like
`src/main/java` need no configuration.

## Badge

```markdown
![coverage](https://gocov.example/badge/myworkspace/myrepo.svg)
```

Red below 50%, yellow 50–75%, green above 75%. Shows the latest upload on
the repo's default branch.

## API

`POST /api/v1/upload` — multipart form, `Authorization: Bearer <token>`

| part      | meaning                                        |
|-----------|------------------------------------------------|
| `profile` | file: the coverage profile                     |
| `repo`    | optional; must match the token's repo          |
| `commit`  | required commit SHA                            |
| `branch`  | defaults to the repo's default branch          |
| `pr_id`   | optional pull request id                       |
| `format`  | `go`, `lcov`, `jacoco` or `cobertura`; omitted → detected from content |
| `path_prefix` | maps profile paths to repo paths for diff coverage, e.g. the Go module path (the CLI fills it from go.mod) |

Returns `201` with `{id, total_pct, covered_stmts, total_stmts,
delta_pct, build_status}`. Uploads carrying a `pr_id` additionally get
`diff_pct`, `diff_covered_lines`, `diff_total_lines`, `diff_status` and
`pr_comment` when the repo has forge credentials configured.

## Configuration

| variable                       | default                 |                             |
|--------------------------------|-------------------------|-----------------------------|
| `DATABASE_URL`                 | —                       | Postgres DSN (required)     |
| `GOCOV_ADDR`                   | `:8080`                 | listen address              |
| `GOCOV_BASE_URL`               | `http://localhost:8080` | public URL used in statuses |
| `GOCOV_BITBUCKET_USERNAME`     | —                       | global Bitbucket bot account (with an API token, the account email) |
| `GOCOV_BITBUCKET_APP_PASSWORD` | —                       | the bot's app password or scoped API token |
| `GOCOV_OAUTH_BITBUCKET_KEY`    | —                       | OAuth consumer key; with the secret, turns on web UI sign-in |
| `GOCOV_OAUTH_BITBUCKET_SECRET` | —                       | OAuth consumer secret       |
| `GOCOV_ALLOWED_WORKSPACES`     | derived from tracked repos | comma-separated workspace slugs allowed to sign in |

The global bot account is used by every repo that has no credentials of
its own — for build statuses, PR comments, diff coverage and default
branch detection. Per-repo credentials (`repo update -bb-username ...`)
take precedence.

### Bitbucket token permissions

The bot credential (a scoped API token, or a legacy app password) needs:

| capability | API token scopes | app password checkboxes |
|---|---|---|
| build status, Code Insights report + annotations, source view, default branch | `read:repository:bitbucket`, `write:repository:bitbucket` | Repositories: Read, Write |
| PR diff coverage, PR comment | `read:pullrequest:bitbucket`, `write:pullrequest:bitbucket` | Pull requests: Read, Write |
| updating the PR comment in place | `read:user:bitbucket` | Account: Read |

Without the account/user scope everything still works, but gocov cannot
recognize its own earlier comment, so every upload posts a **new** PR
comment instead of updating the existing one. If comments stack, this
scope is the fix.

The OAuth consumer used for web UI sign-in is separate and needs the
**Account: Read** and **Email** permissions on the consumer itself.

## Development

```sh
go test ./...
go build ./...
```

The store, forge and blobstore interfaces each have test doubles
(`internal/store/memory`, `internal/forge/fake`,
`internal/blobstore/memory`), so handlers are fully testable without
Postgres or Bitbucket.

The Postgres store additionally has integration tests that run against a
real server when `GOCOV_TEST_DATABASE_URL` is set (they are skipped
otherwise). Each test creates and drops its own scratch database:

```sh
docker run --rm -d --name gocov-test-db -p 5433:5432 \
  -e POSTGRES_USER=gocov -e POSTGRES_PASSWORD=gocov -e POSTGRES_DB=gocov \
  postgres:16-alpine
GOCOV_TEST_DATABASE_URL=postgres://gocov:gocov@localhost:5433/gocov go test ./...
docker stop gocov-test-db
```

`GET /healthz` reports readiness (checks database connectivity) for load
balancers and container orchestrators; the server shuts down gracefully
on SIGINT/SIGTERM.

## License

AGPL-3.0 — see [LICENSE](LICENSE).
