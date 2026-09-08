# API & badge

Most uploads should go through the [CLI](cli.md) or its CI wrappers; this is the HTTP endpoint underneath, for
anything they don't cover.

## Upload

`POST /api/v1/upload` — multipart form, `Authorization: Bearer <token>`

| part          | meaning                                                                                                                                                                                                                                                         |
|---------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `profile`     | file: the coverage profile                                                                                                                                                                                                                                      |
| `repo`        | optional; must match the token's repo                                                                                                                                                                                                                           |
| `commit`      | required commit SHA                                                                                                                                                                                                                                             |
| `branch`      | defaults to the repo's default branch                                                                                                                                                                                                                           |
| `pr_id`       | optional pull request id                                                                                                                                                                                                                                        |
| `format`      | `go`, `lcov`, `jacoco`, `cobertura`, `clover` or `simplecov`; omitted → detected from content                                                                                                                                                                   |
| `path_prefix` | maps profile paths to repo paths for diff coverage, e.g. the Go module path (the CLI fills it from go.mod)                                                                                                                                                      |
| `part`        | optional; names one slice of the commit's coverage (`backend`, `frontend`, `e2e`, …) uploaded from a separate CI job. Normalized to a lowercase slug (`[a-z0-9._-]`, ≤64); omitted or blank → `default`. Re-uploading a part replaces it. See [Parts](parts.md) |
| `ignore`      | optional, repeatable; a glob pattern (or a comma/newline-separated list) for files to leave out of this upload, applied on top of the repo's own patterns. At most 100 patterns of 200 characters; an invalid one is a `400`. See [Ignoring files](ignoring-files.md)                |

Returns `201` with `{id, total_pct, covered_stmts, total_stmts,
delta_pct, build_status}`, plus `ignored_files` when ignore patterns dropped any. Uploads carrying a `pr_id` additionally get
`diff_pct`, `diff_covered_lines`, `diff_total_lines`, `diff_status` and
`pr_comment` when the repo's workspace is [connected to its forge](connecting.md).

### OIDC uploads

A request **without** the `Authorization` header may instead carry a forge-minted OIDC identity token in an
`oidc_token` form part. The server verifies the token's signature against the forge's published keys and checks:

- the issuer is one it trusts (GitHub Actions, Bitbucket Pipelines, and GitLab CI — plus any self-managed GitLab
  issuers the operator configured);
- the audience equals this server's URL — a token minted for another audience is rejected;
- the repository claim falls under a registered workspace on that forge, and the request's own `repo` part (if
  sent) agrees with it. A repo not yet tracked is registered by this upload, as a workspace token's first upload
  would register it; the workspace itself must already exist.

On success the upload proceeds like a token upload — fully verified, published through the workspace's forge
connection. Refusals are explicit: `403 oidc_bad_audience`, `403 oidc_unknown_issuer`, `403 oidc_repo_mismatch`,
`401 oidc_invalid_token`, and `404` for a repo under no registered workspace. See
[GitHub Actions](github-actions.md#uploading-without-a-token).

### Tokenless fork-PR uploads

A request **without** the `Authorization` header or an `oidc_token` may instead authenticate as a running GitHub Actions `pull_request`
workflow — the [fork-PR path](pull-requests.md#fork-prs-without-a-token). It carries three extra parts, and `repo`,
`commit` (the PR head SHA) and `pr_id` become required:

| part          | meaning                                                          |
|---------------|------------------------------------------------------------------|
| `run_id`      | `$GITHUB_RUN_ID` of the workflow run doing the upload            |
| `run_attempt` | `$GITHUB_RUN_ATTEMPT`                                            |
| `head_repo`   | the fork the PR head is on (`pull_request.head.repo.full_name`)  |

The server verifies the claim with GitHub through the repo's App installation; the repo must be public, tracked, and
its workspace connected. Refusals are explicit: `403` with the reason, `404` for an untracked repo, `409` when the
same `(run_id, run_attempt, part)` already uploaded, `429` past the per-repo hourly limit.

## Badge

```markdown
[![coverage](https://app.gocov.dev/badge/github/myworkspace/myrepo.svg)](https://app.gocov.dev/repos/github/myworkspace/myrepo?ref=badge)
```

The link lands on the repo's report page — for a public repo that is a [page anyone can read](public-reports.md);
on a private repo, visitors get the sign-in page. The path names the forge first (`github`, `gitlab` or
`bitbucket`), then the workspace and repo as the forge spells them: a project mirrored to two forges has two report
pages. A self-hosted instance serves the same paths from its own host. Either way there is nothing to assemble by
hand: the repo page shows the finished snippet with a copy button.

Red below 50%, yellow 50–75%, green above 75%. Shows the latest upload on the repo's default branch. Badges are served
without authentication even when web UI sign-in is enabled.
