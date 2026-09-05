# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

gocov is a self-hostable coverage-tracking service (Coveralls/Codecov alternative): a single Go binary + Postgres, AGPL-3.0. Direct dependencies are pgx and caarlos0/env (tag-based env parsing, itself dependency-free); everything else is stdlib.

Package-specific conventions live in `.claude/rules/` and load when you touch the matching paths (`core`, `server`, `forge`, `config`, `store`, `docs`). Slash commands: `/release`, `/verify-release`, `/check-pins`, `/docs-check`.

## Commands

```sh
go build ./...
go vet ./...
go test ./...                               # unit tests only — no Postgres needed
go test ./internal/server -run TestUpload   # single test
```

Postgres integration tests (in `internal/store/postgres`) are skipped unless `GOCOV_TEST_DATABASE_URL` is set; each test creates and drops its own scratch database via `internal/testpg`:

```sh
docker run --rm -d --name gocov-test-db -p 5433:5432 \
  -e POSTGRES_USER=gocov -e POSTGRES_PASSWORD=gocov -e POSTGRES_DB=gocov postgres:18-alpine
GOCOV_TEST_DATABASE_URL=postgres://gocov:gocov@localhost:5433/gocov go test ./...
```

For eyeballing UI changes without Postgres or OAuth, run the dev harness: `go run ./cmd/gocov-preview` serves the web UI from an in-memory store seeded with synthetic history (`GOCOV_PREVIEW_AUTH=1` adds fake sign-in so login/registration/settings pages are previewable). `docker compose up` runs the real thing.

CI (`.github/workflows/ci.yml`) runs vet + tests with a Postgres service and builds the docs site strictly; there is no separate linter.

## Architecture

Three binaries in `cmd/`:
- `gocov-server` — API + web UI, configured entirely via environment variables.
- `gocov` — the upload CLI users run in CI. Detects the coverage format from file content (`detect.go`); defaults to the hosted server URL in `internal/hosted`.
- `gocov-preview` — throwaway dev harness, not part of the product.

Everything hangs off four interfaces, each with a production implementation and a test double, so handlers are fully testable without Postgres or a real forge:

| Interface | Production | Test double |
|---|---|---|
| `store.Store` (normalized coverage model, workspaces/repos/gates) | `store/postgres` | `store/memory` |
| `forge.Forge` (build statuses, PR comments, check runs) | `forge/{bitbucket,github,gitlab}` | `forge/fake` |
| `blobstore.Store` (raw uploaded profiles) | `blobstore/postgres` | `blobstore/memory` |
| `profile.Parser` (one per format: go, lcov, jacoco, cobertura, clover, simplecov) | `internal/profile` | — |

New formats, forges, or storage backends slot in behind these interfaces. The line that matters most: `internal/core` is the coverage pipeline and imports no HTTP (a test enforces it); `internal/server` is the transport and web UI around it. `internal/diffcov` computes diff coverage against forge-fetched diffs, `internal/auth` handles OAuth sign-in per forge, `internal/secretbox` encrypts stored grant refresh tokens, and `internal/config` is the single declaration of every environment variable. SQL migrations are numbered files under `internal/store/postgres/migrations/`.

The `Hosted` config flag switches the instance to self-service mode (any forge account may sign in and register workspaces); the default private mode restricts sign-in to members of tracked workspaces.

## Docs

User-facing docs are in `docs/` and double as the docs.gocov.dev site. Update the relevant page when changing user-visible behavior; user pages never mention environment variables or deployment. The full layout and the site build rules are in `.claude/rules/docs.md`.
