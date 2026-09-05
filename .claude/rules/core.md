---
paths:
  - "internal/core/**"
---

# internal/core — the coverage pipeline, no transport

`internal/core` is the coverage logic, with no HTTP in it: `Accept` runs an upload through the whole pipeline — store the raw profile, measure diff coverage against the PR, evaluate the gate, persist the rows, merge the commit's "parts" (multiple uploads per commit) into a per-commit merged report, then push status/PR comment/check run to the forge. `gate.go`, `merge.go`, `publish.go` and `repo.go` are the pieces it is made of.

`forges.go` owns the deployment's forge connections and their upkeep — resolving the client a repo's workspace is connected through, refreshing a grant and persisting the rotated refresh token (under the store's `WithGrantLock`, a Postgres advisory lock, because Bitbucket and GitLab rotate the token on every use and production runs two tasks during a rolling deploy), caching access tokens in memory, and marking a connection broken when the forge says it is gone.

The package imports neither `net/http` nor `html/template` and `TestCoreImportsNoTransport` fails the build if that changes: anything needing a request or a template belongs on the other side of the line, in `internal/server`.

Every upload re-reads and re-merges all parts for the commit under a lock — see `maxPartsPerCommit` in `internal/server/upload.go` before changing the merge path.
