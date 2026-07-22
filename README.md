# gocov

Self-hostable coverage tracking for Go projects — an open-source
Coveralls/Codecov alternative. Single binary + Postgres. Bitbucket Cloud
is the first supported forge.

## Features (MVP)

- Parses Go cover profiles (`go test -coverprofile`) into total and
  per-file statement coverage
- `POST /api/v1/upload` API with per-repo Bearer tokens
- SVG coverage badge per repo (`/badge/{workspace}/{repo}.svg`)
- Web UI: repo list → upload list → per-file coverage table
- Uploader CLI that auto-detects Bitbucket Pipelines environment
  variables and falls back to git
- Pushes a `coverage: X% (±Y%)` build status to Bitbucket commits when
  the repo has an app password configured
- Diff coverage for pull requests: fetches the PR diff from Bitbucket,
  intersects changed lines with coverage blocks, and posts a PR comment
  listing uncovered changed lines

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

Register a repo and get its upload token:

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
```

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
| `format`  | profile format, default `go`                   |
| `path_prefix` | maps profile paths to repo paths for diff coverage, e.g. the Go module path (the CLI fills it from go.mod) |

Returns `201` with `{id, total_pct, covered_stmts, total_stmts,
delta_pct, build_status}`. Uploads carrying a `pr_id` additionally get
`diff_pct`, `diff_covered_lines`, `diff_total_lines`, `diff_status` and
`pr_comment` when the repo has forge credentials configured.

## Configuration

| variable         | default                 |                             |
|------------------|-------------------------|-----------------------------|
| `DATABASE_URL`   | —                       | Postgres DSN (required)     |
| `GOCOV_ADDR`     | `:8080`                 | listen address              |
| `GOCOV_BASE_URL` | `http://localhost:8080` | public URL used in statuses |

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
