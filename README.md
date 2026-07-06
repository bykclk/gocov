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

## Uploading coverage from CI

In Bitbucket Pipelines (commit, branch, repo and PR id are auto-detected):

```yaml
- step:
    script:
      - go test ./... -covermode=atomic -coverprofile=coverage.out
      - go run github.com/bykclk/gocov/cmd/gocov@latest upload coverage.out
```

with `GOCOV_SERVER` and `GOCOV_TOKEN` set as repository variables.
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

Returns `201` with `{id, total_pct, covered_stmts, total_stmts,
delta_pct, build_status}`.

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

## License

AGPL-3.0 — see [LICENSE](LICENSE).
