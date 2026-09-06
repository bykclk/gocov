# Configuration

| variable                       | default                    |                                                                                                                                                              |
|--------------------------------|----------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `DATABASE_URL`                 | —                          | Postgres DSN (required)                                                                                                                                      |
| `GOCOV_ADDR`                   | `:8080`                    | listen address                                                                                                                                               |
| `GOCOV_BASE_URL`               | `http://localhost:8080`    | public URL used in statuses                                                                                                                                  |
| `GOCOV_GITHUB_APP_ID`          | —                          | GitHub App id; with the key, enables one-click workspace connect                                                                                             |
| `GOCOV_GITHUB_APP_PRIVATE_KEY` | —                          | the App's private key: PEM content, or a path to the PEM file                                                                                                |
| `GOCOV_GITHUB_WEBHOOK_SECRET`  | —                          | verifies GitHub App webhook deliveries (required for a Marketplace listing; optional otherwise)                                                              |
| `GOCOV_SECRET_KEY`             | —                          | at-rest encryption key (exactly 64 hex characters, `openssl rand -hex 32`); with the Bitbucket/GitLab OAuth credentials, enables one-click workspace connect |
| `GOCOV_OAUTH_BITBUCKET_KEY`    | —                          | Bitbucket OAuth consumer key; with the secret, turns on web UI sign-in                                                                                       |
| `GOCOV_OAUTH_BITBUCKET_SECRET` | —                          | Bitbucket OAuth consumer secret                                                                                                                              |
| `GOCOV_OAUTH_GITHUB_KEY`       | —                          | GitHub OAuth app client id; with the secret, turns on web UI sign-in                                                                                         |
| `GOCOV_OAUTH_GITHUB_SECRET`    | —                          | GitHub OAuth app client secret                                                                                                                               |
| `GOCOV_OAUTH_GITLAB_KEY`       | —                          | GitLab OAuth application id; with the secret, turns on web UI sign-in                                                                                        |
| `GOCOV_OAUTH_GITLAB_SECRET`    | —                          | GitLab OAuth application secret                                                                                                                              |
| `GOCOV_ALLOWED_WORKSPACES`     | derived from tracked repos | comma-separated workspace/org slugs allowed to sign in                                                                                                       |
| `GOCOV_MODE`                   | `private`                  | `hosted` opens sign-in to any forge account with self-service workspace registration                                                                         |
| `GOCOV_PUBLIC_REPORTS`         | `on`                       | `off` disables the anonymous read-only report pages that public repos otherwise get                                                                          |
| `GOCOV_OIDC_ISSUERS`           | —                          | comma-separated trusted GitLab issuer(s) for tokenless uploads (self-managed instance URLs, https); replaces the gitlab.com default. GitHub and Bitbucket are always trusted |
| `GOCOV_POSTHOG_KEY`            | —                          | PostHog project API key; when set, the web UI loads PostHog's browser snippet (no cookie; session replay only on the sign-in and setup pages, inputs masked). Unset, the pages load nothing off-site |
| `GOCOV_POSTHOG_HOST`           | `https://eu.i.posthog.com` | PostHog ingestion host the snippet loads from and reports to (the EU cloud by default; a self-hosted PostHog works too)                                    |

How the pieces fit together:

- The OAuth key/secret pairs enable [web UI sign-in](sign-in.md); each provider only needs read scopes for sign-in.
- [One-click forge connections](forge-connections.md) — what turns on statuses, PR comments, check runs, diff coverage
  and source view — need `GOCOV_SECRET_KEY` plus the Bitbucket or GitLab OAuth credentials for those two forges; on
  GitHub they need `GOCOV_GITHUB_APP_ID` plus the private key, and no `GOCOV_SECRET_KEY`.
- `GOCOV_ALLOWED_WORKSPACES` and `GOCOV_MODE` shape
  [who can sign in and register workspaces](sign-in.md).

A repo whose workspace has no forge connection still stores and reports coverage — the forge surfaces are simply
skipped.
