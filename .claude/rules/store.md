---
paths:
  - "internal/store/**"
  - "internal/blobstore/**"
  - "internal/testpg/**"
---

# Store, blob store and migrations

`store.Store` (normalized coverage model, workspaces/repos/gates) has `store/postgres` in production and `store/memory` as the test double; `blobstore.Store` (raw uploaded profiles) has `blobstore/postgres` and `blobstore/memory`. A change to an interface lands in both implementations and in the tests that drive them.

SQL migrations live in `internal/store/postgres/migrations/` as numbered `00NN_name.sql` files, embedded and applied automatically at boot. New schema changes get the next number; never edit a migration that has shipped. Migrations are forward-only in production, so a rollback leaves the schema behind.

Postgres integration tests in `internal/store/postgres` are skipped unless `GOCOV_TEST_DATABASE_URL` is set; each test creates and drops its own scratch database via `internal/testpg`, which fails (not skips) when the variable is set but the database is unreachable. The local test database is the `docker run` line in CLAUDE.md; the session hook exports the variable automatically when it is listening on :5433.
