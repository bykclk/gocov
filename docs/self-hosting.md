# Self-hosting in production

To evaluate gocov on your own machine, `docker compose up` in the repo brings Postgres and the server up on
http://localhost:8080, migrations apply themselves, and from there [Getting started](getting-started.md) is the same
as on the hosted service — a fresh instance just needs [sign-in enabled](sign-in.md) first, so onboarding has a forge
identity to derive your workspaces from.

This page is the difference between that and an instance other people upload to: TLS in front, a database you did not
start with the app, a key you can lose, and upgrades that have to be boring. The process itself stays small — almost
everything here is about what surrounds it.

## What you are running

One process and one Postgres. Everything gocov persists lives in Postgres — coverage history, workspaces, repos, gates,
sessions, and the raw uploaded profiles themselves, because the blob store is a Postgres table too. The container writes
nothing to disk, so it can be rebuilt or replaced without moving state, and there is exactly one thing to back up.

The image is distroless: a static binary, CA certificates and nothing else — no shell, no package manager — running as
uid **65532**. Every release publishes it to `ghcr.io/gocov/gocov-server` for amd64 and arm64, and it is the same image
the hosted instance runs. Pin a version rather than `latest` — an upgrade should be something you chose, with the
release notes read, not something a restart did to you:

<!-- x-release-please-start-version -->
```yaml
services:
  server:
    image: ghcr.io/gocov/gocov-server:v0.23.0
```
<!-- x-release-please-end -->

The root `docker-compose.yml` builds from the repo instead, because the evaluation case is often "the code I just
changed". If you would rather run the binary under systemd, every release ships `gocov-server` for linux, darwin and
windows on amd64 and arm64, with `checksums.txt` alongside.

All of it is AGPL-3.0. The server contacts nothing but your database and the forge APIs — no telemetry, no license
check, no call home — so a running deployment depends on this project only for the next version you choose to run.
The web UI loads nothing off-site either, unless you opt in: setting `GOCOV_POSTHOG_KEY` adds PostHog's browser
snippet to the pages (no cookie; session replay only on the sign-in and setup pages with inputs masked; `GOCOV_POSTHOG_HOST`
defaults to the EU cloud), which is how
gocov's own hosted instance counts page views. Leave it unset and nothing changes.

The footprint is modest. gocov's own hosted instance is one 0.5 vCPU / 1 GB arm64 container behind a managed load
balancer, in front of a 2 vCPU / 1 GB managed Postgres — and until it moved there it ran the compose file below on a
single 2 vCPU / 2 GB VM.

The repo ships a starting point for this shape under `deploy/`: `docker-compose.prod.yml` pulls the published image at
the version pinned in `.env` and runs it behind a Caddy TLS terminator, expecting Postgres to be external; the
`Caddyfile` next to it is the one quoted below. The root `docker-compose.yml` is the evaluation stack and is not the
same thing — it brings its own Postgres and builds from source; every release brings it up from the freshly published
image instead and checks it answers, so the compose path does not rot.

## TLS and the reverse proxy

The server speaks plain HTTP on `GOCOV_ADDR` (`:8080` by default), so terminate TLS in front of it.

Set `GOCOV_BASE_URL` to the public `https://` URL. This is not cosmetic: gocov builds every outgoing link from it —
build statuses, PR comments, badges — and auth cookies are marked `Secure` exactly when it starts with `https://`. An
instance served over TLS whose base URL still says `http://` hands out session cookies without the `Secure` attribute.

Because outgoing URLs come from that one variable, gocov never has to trust `X-Forwarded-Proto` or `X-Forwarded-Host`.
There is no proxy-header configuration to get wrong.

One proxy setting does matter: the request body limit. The server caps an upload at **64 MiB**; a proxy with a smaller
limit (nginx defaults to 1 MB) rejects large reports before gocov ever sees them, and the CLI surfaces the proxy's error
instead of a gocov one. Allow at least 64 MiB — `client_max_body_size 64m` on nginx, or with Caddy:

```caddyfile
gocov.example.com {
	request_body {
		max_size 100MB
	}
	reverse_proxy server:8080
	encode gzip

	header {
		Strict-Transport-Security "max-age=31536000"
		X-Content-Type-Options "nosniff"
	}
}
```

## Postgres

Bring your own: the DSN goes in `DATABASE_URL`, and connection options such as `sslmode=require` go in the URL. The
Postgres in `docker-compose.yml` is for evaluation only — fixed development credentials, a container volume, the same
host as the app.

Migrations are embedded in the binary and applied at start-up, in filename order, tracked in a `schema_migrations`
table. A deploy is therefore "new image, restart", never a separate migrate step — but the database user does need DDL
rights on its schema, not just read/write.

They are forward-only: there are no down migrations. Rolling the binary back does not roll the schema back, so crossing
a schema change in reverse means restoring from a backup.

## The secret key

`GOCOV_SECRET_KEY` is exactly 64 hex characters — `openssl rand -hex 32`. It is raw AES-256 key material, not a
passphrase: a memorable string is refused at boot rather than stretched by a KDF, and a malformed value stops the
process instead of degrading a feature.

It encrypts Bitbucket and GitLab grant refresh tokens at rest (AES-256-GCM). GitHub App connections do not use it —
those ride the App's private key.

Losing or rotating the key does not lose coverage data. A refresh token that no longer decrypts comes back marked
broken, the workspace shows as needing a reconnect, and reconnecting re-seals it under the current key. The cost of
rotation is one reconnect per Bitbucket/GitLab workspace; there is no re-encryption tool and none is needed.

Back the key up, and not only inside the database dump. A dump restored without its key is a dump every connected
workspace has to reconnect by hand.

## The GitHub App private key

`GOCOV_GITHUB_APP_PRIVATE_KEY` takes either PEM content or a path to the PEM file. In a container the path plus a
read-only mount is easier to live with than a multi-line environment variable.

The container runs as uid 65532 and the server exits when it cannot read a key it was configured with, so wrong
ownership is a crash loop rather than a missing feature. Keep the file `600` and give it to that uid instead of opening
it up — the key can mint installation tokens for every repo the App is installed on:

```sh
sudo chown 65532 github-app.pem
```

Check it without touching the running service:

```sh
docker run --rm --user 65532:65532 -v "$PWD/github-app.pem:/run/k.pem:ro" alpine:3.21 cat /run/k.pem > /dev/null && echo readable
```

## Tokenless uploads from a self-managed GitLab

Tokenless [OIDC uploads](cli.md#uploading-without-a-token) work out of the box for GitHub Actions, Bitbucket
Pipelines and gitlab.com — gocov trusts those issuers already. A **self-managed GitLab** issues its OIDC tokens
under its own instance URL, so gocov will not trust them until you say so: list that issuer (the instance's base
URL, e.g. `https://gitlab.example.com`, https only) in [`GOCOV_OIDC_ISSUERS`](configuration.md).

Setting it **replaces** the gitlab.com default rather than adding to it — a gocov deployment connects to a single
GitLab, so it trusts that one instance's issuer. (Trusting gitlab.com and a self-managed instance at once would let
a token from either authenticate an upload to a same-named project on the other, since GitLab tokens name the
project by path and paths are not unique across instances.) GitHub and Bitbucket are always trusted, independently.
The token's `aud` must still be this server's `GOCOV_BASE_URL`, so a token minted for another instance cannot be
replayed here.

## Health checks

`GET /healthz` reports readiness (it checks database connectivity) for load balancers and container orchestrators; it
stays open when sign-in is enabled.

The container image is distroless, so it has no `wget` or `curl` for a Docker `HEALTHCHECK` to call. The binary probes
itself instead — `gocov-server healthcheck` requests `/healthz` on `GOCOV_ADDR` and exits non-zero if it is not
`200 OK`, which is what the compose files use. Three timeouts are nested inside each other and want to stay in that
order: the 2s `/healthz` spends on its database ping, the 2.5s the probe waits for a reply, and the 3s Docker allows
the probe to run.

```yaml
healthcheck:
  test: ["CMD", "/usr/local/bin/gocov-server", "healthcheck"]
```

## Upgrading

Point at the new image, restart; migrations apply themselves on the way up. On the compose deployment above that is
editing `GOCOV_VERSION` in `.env` (read the release notes first — they are where a breaking change is announced) and:

```sh
docker compose -f docker-compose.prod.yml pull && docker compose -f docker-compose.prod.yml up -d
```

Detached, because this is the long-running instance — unlike the foreground `docker compose up` the quick start uses to
show you the boot log.

A restart is the one place the shutdown budget shows. `SIGTERM` drains in-flight requests for up to 15 seconds, which
is longer than Docker's 10-second default stop timeout — raise `stop_grace_period` above it, or a rolling restart will
cut an upload in half. Kubernetes' 30-second default already clears it.

`gocov-server version` reports what is actually running — the published image is stamped with its release tag, and a
build of your own derives the version from git when
the image is built.

The server and the upload CLI version independently, so pin the CLI in CI on its own schedule; see
[Other CI systems](ci-other.md).

## What talks to what

| direction    | traffic                                                                     |
|--------------|-----------------------------------------------------------------------------|
| inbound 443  | browsers, CI uploads, and GitHub App webhook deliveries if you enabled them |
| outbound 443 | forge APIs — statuses, PR comments, check runs, diffs                       |
| Postgres     | from the server only; it never needs to be reachable from anywhere else     |

Every variable named on this page, and the rest of them, are listed in [Configuration](configuration.md). Setting up
the forge side is [Forge apps & credentials](forge-connections.md); who may sign in is [Sign-in](sign-in.md).
