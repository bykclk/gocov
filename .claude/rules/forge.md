---
paths:
  - "internal/forge/**"
  - "internal/rest/**"
  - "internal/auth/**"
---

# Forge clients, sign-in providers and the shared REST plumbing

No forge-specific types or URLs may leak out of the concrete forge implementations (`internal/forge/{bitbucket,github,gitlab}`); the rest of the codebase sees only `forge.Forge` and its sentinels, and `forge/fake` is the test double.

The forge packages and the `internal/auth` sign-in providers share their request plumbing through `internal/rest` (build, authorize, bounded reads, pagination, uniform `*rest.Error` on a refused answer). A forge client keeps only what its API means — paths, payloads, which status maps to which sentinel. New request-level behaviour (retries, size limits, pagination shapes) belongs in `rest`, not copied into a client.

Grant refresh and token caching are not the client's job: `internal/core/forges.go` refreshes a grant, persists the rotated refresh token under the store's `WithGrantLock`, and marks a connection broken when the forge says it is gone. A client reports the condition; core decides what to do with it.
