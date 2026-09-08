# gocov documentation

gocov tracks your test coverage and puts it where your team already works: a build status and delta on every
commit, a diff-coverage comment on every pull request, a gate that can block merges, and a badge for the README.
It works with GitHub, Bitbucket Cloud and GitLab, and accepts coverage reports from Go, JavaScript/TypeScript,
Java/Kotlin, Python, PHP and Ruby — the format is detected automatically.

Use the hosted service at [app.gocov.dev](https://app.gocov.dev/?ref=docs), or
[run your own instance](self-hosting.md) — same binary, same docs.

## Start here

1. **[Getting started](getting-started.md)** — sign in, add one CI step, and coverage lands on your next push.
2. **Wire up your CI** — [GitHub Actions](github-actions.md), [GitLab CI](gitlab-ci.md),
   [Bitbucket Pipelines](bitbucket-pipelines.md), or [any other CI](ci-other.md).
3. **[Your language](languages.md)** — the test command and upload line for each supported ecosystem.

Coming from Codecov or Coveralls? [Migrating](migrating.md) maps their settings onto gocov's and gives the order
that keeps the old tool running until the numbers agree.

## Guides

| Page                                        | Covers                                                              |
|---------------------------------------------|---------------------------------------------------------------------|
| [Pull requests](pull-requests.md)           | The comment, check run, inline annotations and source view          |
| [Connect your forge](connecting.md)         | The one-click connection that turns the PR surfaces on              |
| [Coverage gate](coverage-gate.md)           | Minimum/diff/drop rules and making them block merges                |
| [Parts](parts.md)                           | Combining reports from separate CI jobs into one per commit         |
| [Ignoring files](ignoring-files.md)         | Leaving generated code, mocks and dev tooling out of the numbers    |
| [Public reports](public-reports.md)         | Read-only report pages anyone can open, for public repos            |
| [Why coverage changed](coverage-changed.md) | Baselines, missing parts, empty diff coverage — the usual surprises |

## Reference

- [CLI](cli.md) — every flag and environment variable of `gocov upload`
- [API & badge](api.md) — the raw upload endpoint and the SVG badge

## Self-hosting

Everything about running an instance yourself lives in its own section:
[production setup](self-hosting.md), [sign-in](sign-in.md), [forge apps & credentials](forge-connections.md)
and the [configuration reference](configuration.md). None of it is needed to *use* gocov.

## Conventions

Throughout these docs, *forge* means the code host (Bitbucket Cloud, GitHub or GitLab) and *workspace* means the
namespace gocov tracks — a Bitbucket workspace, a GitHub org or user, or a GitLab group, subgroup (by full path) or user
namespace.
