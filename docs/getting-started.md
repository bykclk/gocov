# Getting started

By the end of this page a push carries its own coverage: a percentage and a delta on the commit, a comment on the
pull request, a gate that can hold a merge, and a badge for the README. Three steps get you there, and none of them
involves pasting a secret.

## 1. Sign in and claim your workspace

Sign in at [app.gocov.dev](https://app.gocov.dev/?ref=docs) with your forge account (GitHub, Bitbucket or GitLab).

If you belong to no workspace gocov already tracks, you land on **/register**, which lists the workspaces your forge
account is a member of. Claiming one creates it and makes you its owner. Claiming takes an admin's role on the forge
(org admin, group Owner, workspace administrator); only workspaces the forge itself reports for your account can be
claimed, so there is nothing to dispute: if a colleague registered yours first, signing in simply makes you a member.

A *workspace* is the namespace gocov tracks — a GitHub org or user, a GitLab group, a Bitbucket workspace. Repos under
it register themselves on their first upload, so the workspace is the only thing you create by hand.

## 2. Connect the workspace to its forge

One click, on the workspace's setup page: **Connect** (a GitHub App install, or a Bitbucket/GitLab consent). On
GitHub, claiming and connecting are the same install. This is what turns on everything your team sees on the forge —
the build status, the PR comment, the check run, diff coverage — and it is what lets your CI upload without a token:
the server verifies each job's identity token against this connection. [Connect your forge](connecting.md) has the
details and the exact list of what it enables.

## 3. Add the upload step to CI

One step after your tests. For GitHub Actions, grant the job `id-token: write` and it needs no secret at all:

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - run: go test ./... -covermode=atomic -coverprofile=coverage.out
  - uses: gocov/gocov-action@v1
    with:
      files: coverage.out
```

The job asks GitHub for a short-lived, signed identity token that proves which repository it is; gocov verifies it
and accepts the upload. Nothing to create, nothing to paste, nothing to rotate. Bitbucket Pipelines and GitLab CI
have the same thing — `oidc.audiences` on the step, an `id_tokens` entry (or the gocov component) in the job — and
the setup page shows the snippet for your forge with everything filled in. The recipes for each CI and each language:

- [GitHub Actions](github-actions.md) · [GitLab CI](gitlab-ci.md) · [Bitbucket Pipelines](bitbucket-pipelines.md) ·
  [other CI systems](ci-other.md)
- [Languages & formats](languages.md) — what to upload from Jest, JaCoCo, pytest, PHPUnit, SimpleCov, …

If your CI cannot mint identity tokens, or you would rather use a secret, the workspace has an **upload token** —
owners see it on the setup page and can rotate it from settings. Set it as a workspace-level CI variable named
`GOCOV_TOKEN` (secured) and pass it to the upload step; a pasted token always takes precedence over the identity
token. Every recipe above shows both forms.

The first upload registers the repo. The setup page shows a live "waiting for your first upload" state that turns
into the repo link once coverage arrives.

![The onboarding page once the first profile has arrived: the three setup steps checked off, and the repository's first report](assets/onboarding.png)

## What you have now

The dashboard lists every repo that has uploaded, with its coverage, its trend and whether its gate is passing.

![The dashboard: workspace coverage, how many gates are passing, and one row per repository with its coverage, delta, 30-day trend and gate](assets/dashboard.png)

Each repo page graphs coverage over time and links every upload; each workspace links to a **settings** page where you
can rotate the upload token (the old one dies immediately, the new one is shown once), set the default branch and the
gate defaults new repos inherit, and connect or disconnect the forge.

![Coverage over time on a repo page: total coverage per upload, gate failures marked in red, and a dashed line at the gate minimum](assets/trend.png)

Who sees what follows your forge: members of the workspace see its repos and nobody else does, with no invite step to
manage. Who changes what follows it too: the forge's admins are the workspace's owners, the only ones who set gates,
connect the forge or see the upload token — see [owners and members](sign-in.md#owners-and-members).

## Next steps

- [Coverage gate](coverage-gate.md) — set minimums and make them block merges
- [Parts](parts.md) — combine several CI jobs into one report per commit
- [API & badge](api.md) — put the badge in your README

## Running your own instance

Everything above works the same on a self-hosted instance — set `GOCOV_SERVER` as a plain CI variable (the identity
token's audience is your instance's URL, so the setup page fills it in) and the snippets are otherwise identical. To try one locally, `docker compose up` brings Postgres and the server up on
http://localhost:8080; [Self-hosting](self-hosting.md) covers that and the path to a production instance.
