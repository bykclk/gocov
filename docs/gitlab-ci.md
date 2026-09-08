# GitLab CI

1. Add `GOCOV_TOKEN` under **Settings → CI/CD → Variables** on the group or project, holding your workspace's
   [upload token](getting-started.md). Mark it **masked**, but **not protected** — GitLab checks "Protect variable" by
   default, and protected variables never reach merge request pipelines. (Or add nothing and
   [upload without a token](#uploading-without-a-token).)

2. Include the gocov component and hand it the coverage file your test job leaves as an artifact:

```yaml
include:
  - component: gitlab.com/gocov/gocov/upload@1
    inputs:
      files: coverage.out
      needs: [test]

workflow:
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH

test:
  image: golang:1.23
  script:
    - go test ./... -covermode=atomic -coverprofile=coverage.out
  artifacts:
    paths: [coverage.out]
```

Only the test command and `files` change for other languages — [Languages & formats](languages.md) lists what each
test tool writes.

The component adds one job, `gocov-upload`, after the jobs named in `needs`. It downloads the pinned gocov CLI release
for the runner's architecture, verifies its sha256 against the release's `checksums.txt`, and uploads every file
matching `files` (comma-separated, globs allowed) — no toolchain to install. Project path, commit, branch and MR iid
are auto-detected, including the real head SHA on merged-results pipelines. The `workflow` rules run the pipeline on
merge requests and the default branch without duplicates.

The inputs, in full: `files`, `needs`, `stage`, `job-name` (include the component once per [part](parts.md), each with
its own name and `part`), `server` (when self-hosting), `part`, `ignore` ([patterns](ignoring-files.md) on top of the
repo's own), `allow-failure`, `version` (the CLI release; the default is the one that component release was tested
against) and `image`. They are documented in the
[component's README](https://gitlab.com/gocov/gocov), which is also where the CI/CD Catalog lists it. `@1` follows
the newest 1.x release; `@1.0.0` pins one.

Note that gitlab.com's free tier requires a verified account (credit card) before shared runners pick up jobs.

## Uploading without a token

GitLab can mint a short-lived, signed OIDC ID token for a job, so you can drop the `GOCOV_TOKEN` variable
entirely. The component's job already declares the `id_tokens` entry it needs, with your gocov server as its `aud`:
leave the variable out and the upload goes via OIDC — the uploader reads the token from `GOCOV_ID_TOKEN` and the
server verifies which project it came from.

The project's workspace (its GitLab group) must already be registered on gocov and [connected](connecting.md) to
GitLab — the same connection that posts the commit status and merge-request note. The project itself needs no
setup: its first OIDC upload registers it, exactly as a token upload would. OIDC replaces only the upload token;
publishing still goes through that connection. A pasted `GOCOV_TOKEN` always takes precedence, so existing setups are untouched, and a rejected
OIDC upload logs the reason and exits 0 rather than failing the build.

Without the component, declare the entry yourself — it must be named `GOCOV_ID_TOKEN`, and the `aud` must equal
your gocov server's URL (`https://app.gocov.dev` on the hosted service, your instance's URL when self-hosting):

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

On a self-managed GitLab, the OIDC tokens are issued under your instance's own URL; the gocov operator has to
trust that issuer before its uploads are accepted — see [self-hosting](self-hosting.md).

## Without the component

A self-managed GitLab cannot include components from gitlab.com's catalog. Either mirror
[the component project](https://gitlab.com/gocov/gocov) into your instance and include it from there, or download
the CLI in the job yourself — the checksum line pins the binary:

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

## Blocking merges on coverage

With the workspace [connected](connecting.md), every upload sets a commit status and posts a diff-coverage note on the
merge request. To make the [coverage gate](coverage-gate.md) block merges, reference the `gocov` commit status under
**Settings → Merge requests → Status checks**.
