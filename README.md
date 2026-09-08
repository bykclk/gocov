# gocov

[![coverage](https://app.gocov.dev/badge/gocov/gocov.svg)](https://app.gocov.dev/repos/gocov/gocov?ref=badge)

Diff coverage, pull request comments and merge gates for GitHub, GitLab and Bitbucket Cloud — an open-source
Coveralls/Codecov alternative. Use the hosted service at [gocov.dev](https://gocov.dev) (public repos free forever,
badge included), or self-host the same product: one Go binary plus Postgres.

![The gocov dashboard: workspace coverage, how many gates are passing, and one row per repository with its coverage, delta, 30-day trend and gate](docs/assets/dashboard.png)

## Coverage on your pull requests in three steps

1. Sign in at [app.gocov.dev](https://app.gocov.dev) and claim your workspace.
2. Connect it to your forge — one click; on GitHub it is the same App install.
3. Add one step after your tests, no secret needed (GitHub Actions shown):

```yaml
permissions:
  id-token: write                 # the job proves its identity; nothing to paste
steps:
  - run: npx jest --coverage      # or go test -coverprofile, mvn verify, pytest --cov, ...
  - uses: gocov/gocov-action@v1
    with:
      files: coverage/lcov.info
```

The job's short-lived identity token replaces the upload secret; a `GOCOV_TOKEN` secret still works where a CI cannot
mint one. The action downloads the CLI binary and checks its sha256, so there is no toolchain to install — the same
lines work whatever your tests are written in; point `files` at whatever your test runner produced. Recipes for the
rest:
[GitLab CI](docs/gitlab-ci.md) · [Bitbucket Pipelines](docs/bitbucket-pipelines.md) ·
[any other CI](docs/ci-other.md) · [languages & formats](docs/languages.md)

From then on every push carries its own coverage: a status and delta on the commit, a diff-coverage comment on the
pull request, a gate that can block the merge, and a badge that is one line of markdown:

```markdown
[![coverage](https://app.gocov.dev/badge/myworkspace/myrepo.svg)](https://app.gocov.dev/repos/myworkspace/myrepo?ref=badge)
```

## What you get

- **Pull request surfaces:** build statuses, PR/MR comments with diff coverage, GitHub check runs and Bitbucket Code
  Insights report cards with inline annotations on uncovered changed lines — see
  [the tour](docs/pull-requests.md)
- **Coverage gate:** per-repo minimums for total and diff coverage plus a drop tolerance; a failed gate pushes a
  FAILED status your forge's merge checks can block on
- **Any language:** Go cover profiles, LCOV (Jest, Vitest, nyc, c8), JaCoCo XML (Maven, Gradle, Android), Cobertura
  XML (coverage.py/pytest-cov, coverlet, gcovr), Clover XML (PHPUnit, Istanbul) and SimpleCov resultsets (Ruby) —
  detected from the uploaded content, no flag needed
- **Web UI:** repo list, per-file coverage, line-by-line source view with hit counts, coverage trend chart, SVG badge
  per repo, sign-in with your forge account

## Self-hosting

The same product on your infrastructure, AGPL-3.0, no telemetry and no call home:

```sh
docker compose up
```

This starts Postgres and the server on http://localhost:8080 (migrations apply automatically). Then
[enable sign-in](docs/sign-in.md) with your forge, sign in — the onboarding wizard registers your workspace and mints
its upload token — and set `GOCOV_TOKEN` and `GOCOV_SERVER` in CI.

For a real instance, every release publishes `ghcr.io/gocov/gocov-server` for amd64 and arm64, and
[deploy/](deploy/README.md) has the production compose file: the pinned image behind Caddy (TLS included), Postgres
external or bundled, one `.env` to fill in. The operator's guide — the secret key, your own GitHub App, backups,
upgrades — is [Self-hosting](docs/self-hosting.md).

## Documentation

The full documentation is at **[docs.gocov.dev](https://docs.gocov.dev)**, built from [docs/](docs/README.md) in this
repository:

- [Getting started](docs/getting-started.md) — sign in, one CI step, first report
- [Migrating from Codecov or Coveralls](docs/migrating.md) — the action inputs, `codecov.yml` and the Coveralls
  settings mapped onto gocov, and the order that keeps the old tool running until the numbers agree
- Uploading from CI — [GitHub Actions](docs/github-actions.md), [GitLab CI](docs/gitlab-ci.md),
  [Bitbucket Pipelines](docs/bitbucket-pipelines.md), [other CI systems](docs/ci-other.md),
  [languages & formats](docs/languages.md)
- [Pull requests](docs/pull-requests.md) · [Connect your forge](docs/connecting.md) ·
  [Coverage gate](docs/coverage-gate.md) · [Parts](docs/parts.md) ·
  [Why coverage changed](docs/coverage-changed.md)
- [CLI](docs/cli.md) · [API & badge](docs/api.md)
- Self-hosting — [production setup](docs/self-hosting.md), [sign-in](docs/sign-in.md),
  [forge apps & credentials](docs/forge-connections.md), [configuration](docs/configuration.md),
  [development](docs/development.md)

## License

AGPL-3.0 — see [LICENSE](LICENSE).
