# GitLab CI

1. Add `GOCOV_TOKEN` under **Settings → CI/CD → Variables** on the group or project, holding your workspace's
   [upload token](getting-started.md). Mark it **masked**, but **not protected** — GitLab checks "Protect variable" by
   default, and protected variables never reach merge request pipelines. When self-hosting, add `GOCOV_SERVER` beside
   it.

2. Add an upload step after your tests:

<!-- x-release-please-start-version -->
```yaml
workflow:
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH

coverage:
  image: golang:1.23
  script:
    - go test ./... -covermode=atomic -coverprofile=coverage.out
    - curl -fsSLO https://github.com/gocov/gocov/releases/download/v0.23.0/gocov-linux-amd64
    - curl -fsSL https://github.com/gocov/gocov/releases/download/v0.23.0/checksums.txt
      | grep ' gocov-linux-amd64$' | sha256sum -c -
    - chmod +x gocov-linux-amd64
    - ./gocov-linux-amd64 upload coverage.out
```
<!-- x-release-please-end -->

Only the test command and the uploaded path change for other languages —
[Languages & formats](languages.md) lists what each test tool writes.

Project path, commit, branch and MR iid are auto-detected, including the real head SHA on merged-results pipelines.
The `workflow` rules run the job on merge requests and the default branch without duplicate pipelines.

Note that gitlab.com's free tier requires a verified account (credit card) before shared runners pick up jobs.

## Uploading without a token

GitLab can mint a short-lived, signed OIDC ID token for a job, so you can drop the `GOCOV_TOKEN` variable
entirely. Declare an `id_tokens` entry named `GOCOV_ID_TOKEN` with your gocov server as its `aud`; the uploader
reads the token from that variable and the server verifies which project it came from.

```yaml
coverage:
  image: golang:1.23
  id_tokens:
    GOCOV_ID_TOKEN:
      aud: https://app.gocov.dev   # your gocov server URL
  script:
    - go test ./... -covermode=atomic -coverprofile=coverage.out
    - go run github.com/gocov/gocov/cmd/gocov@latest upload coverage.out
```

The `aud` must equal your gocov server's URL (`https://app.gocov.dev` on the hosted service, your instance's URL
when self-hosting). The variable must be named `GOCOV_ID_TOKEN` — that is where the uploader looks.

The project's workspace (its GitLab group) must already be registered on gocov and [connected](connecting.md) to
GitLab — the same connection that posts the commit status and merge-request note. The project itself needs no
setup: its first OIDC upload registers it, exactly as a token upload would. OIDC replaces only the upload token;
publishing still goes through that connection. A pasted `GOCOV_TOKEN` always takes precedence, so existing setups are untouched, and a rejected
OIDC upload logs the reason and exits 0 rather than failing the build.

On a self-managed GitLab, the OIDC tokens are issued under your instance's own URL; the gocov operator has to
trust that issuer before its uploads are accepted — see [self-hosting](self-hosting.md).

## Blocking merges on coverage

With the workspace [connected](connecting.md), every upload sets a commit status and posts a diff-coverage note on the
merge request. To make the [coverage gate](coverage-gate.md) block merges, reference the `gocov` commit status under
**Settings → Merge requests → Status checks**.
