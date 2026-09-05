---
paths:
  - "internal/server/**"
---

# internal/server — transport and web UI

`internal/server` is the transport and the web UI around the pipeline: the HTTP API, the SVG badge, and an htmx-based web UI with Go templates and static assets embedded via `go:embed`. `upload.go` authenticates the token, reads and validates the multipart request, and hands a `core.Submission` to the pipeline, which resolves its own forge client. The connect flows (`githubapp.go`, `grant.go` for the Bitbucket and GitLab OAuth grants, `githubwebhook.go`) are the handlers around those connections — consent redirects, callbacks, disconnect — while the tokens behind them live in `core`.

## File layout

Files are named for what they serve — one page or concern each (`page`-style files like `repopage.go`/`uploadpage.go`, `oauth.go`/`session.go`/`scope.go` for identity, `workspace.go`/`register.go`/`repo_settings.go` for administration) — and each has a test file of the same name. The exceptions are deliberate:

- `upload_gate_test.go`, `upload_merge_test.go` and `upload_push_test.go` drive the upload endpoint end to end for behaviour whose unit tests live in `internal/core`;
- `public_reports_test.go` drives the anonymous report pages end to end across the handlers whose access decision lives in `scope.go` and `session.go`;
- `bitbucketgrant_test.go`/`gitlabgrant_test.go` drive `grant.go` once per forge, since the handlers are one table-driven set but each forge's fixture, consent and rotation are its own.

## Gotchas

- Repo slugs contain a slash (`workspace/repo`), so routes use the `{slug...}` wildcard, not `{slug}`. A single-segment pattern still passes `httptest` handler tests but 404s on the live mux — keep new slug routes on `{slug...}` (see the comment near the route table in `server.go`).
- Every upload re-reads and re-merges all parts for the commit under a lock — see `maxPartsPerCommit` in `upload.go` before changing the merge path.
- For eyeballing UI changes without Postgres or OAuth: `go run ./cmd/gocov-preview` (`GOCOV_PREVIEW_AUTH=1` adds fake sign-in). The preview server is configured in `.claude/launch.json`; use the browser preview tools rather than curl to check a rendered page.
