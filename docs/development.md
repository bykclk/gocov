# Development

```sh
go test ./...
go build ./...
```

The store, forge and blobstore interfaces each have test doubles (`internal/store/memory`, `internal/forge/fake`,
`internal/blobstore/memory`), so handlers are fully testable without Postgres or a forge.

The Postgres store additionally has integration tests that run against a real server when `GOCOV_TEST_DATABASE_URL` is
set (they are skipped otherwise). Each test creates and drops its own scratch database:

```sh
docker run --rm -d --name gocov-test-db -p 5433:5432 \
  -e POSTGRES_USER=gocov -e POSTGRES_PASSWORD=gocov -e POSTGRES_DB=gocov \
  postgres:18-alpine
GOCOV_TEST_DATABASE_URL=postgres://gocov:gocov@localhost:5433/gocov go test ./...
docker stop gocov-test-db
```

## Configuration

Every environment variable the binaries read is declared as a tagged struct field in `internal/config`:

```go
type Server struct {
	DatabaseURL       string   `env:"DATABASE_URL,required,notEmpty"`
	Addr              string   `env:"GOCOV_ADDR" envDefault:":8080"`
	AllowedWorkspaces []string `env:"GOCOV_ALLOWED_WORKSPACES" envSeparator:","`
	GitHub            OAuthApp `envPrefix:"GOCOV_OAUTH_GITHUB_"`
	...
}
```

That package is the authoritative list, and a test enforces it: `TestConfigurationDocIsInSync` walks the struct tags
with `env.GetFieldParams` and fails if a variable has no row in [configuration](configuration.md), if a row survives a
variable that was removed, or if a documented default no longer matches the tag. `main` parses and validates
once at start-up and then only reads the struct, so a new setting means a new field there, not another `os.Getenv` at
the point of use. Presence is the tags' job — `required,notEmpty`, because `required` alone
would accept a variable passed through as `""`. Only what the tag vocabulary cannot say is written out: `validate` for
what must stop the process (a malformed `GOCOV_SECRET_KEY`, `GOCOV_MODE=hosted` with no sign-in provider) and
`Warnings` for what is survivable (half a credential pair — logged, feature left off). `LoadServerFrom` parses an explicit environment map
instead of the process environment, so the whole contract is covered by ordinary table tests.

## Extending gocov

Coverage formats sit behind `profile.Parser`, forges behind
`forge.Forge`, and raw profile storage behind `blobstore.Store`; the database schema stores a format-agnostic normalized
model. GitHub and GitLab were each added this way — new formats or S3 storage slot in without rewrites.

## Releasing

A gocov release lands in three repositories. The CLI is tagged here, which builds the binaries and publishes the
release; [gocov-action](https://github.com/gocov/gocov-action) pins the CLI version its users download; and
[upload-pipe](https://github.com/gocov/upload-pipe) bakes the same version into the Bitbucket pipe image.

### Cutting one

Say which version you want, as an empty commit whose message carries a `Release-As:` footer — main only takes pull
requests, so it goes in as one (keep the footer in the squash message when merging):

```sh
git checkout -b release-0.14.0
git commit --allow-empty -m "chore: release 0.14.0" -m "Release-As: 0.14.0"
git push -u origin release-0.14.0
```

[release-please](https://github.com/googleapis/release-please) picks that up and opens a release pull request
holding the version, the CHANGELOG entry and the bumped install-snippet pins. Read it, then merge it: merging tags
`v0.13.0`, and the tag's build publishes the ten binaries and `checksums.txt`, each with a build provenance attestation.

The version is stated rather than inferred on purpose. release-please normally derives it from `feat:`/`fix:`
commit prefixes, and this repo writes commit subjects as English sentences instead — a convention worth more than
the inference is. So it runs as a pull-request, CHANGELOG and pin machine, and nothing happens until a
`Release-As:` commit asks for it. The cost of that trade is a thin CHANGELOG: with no conventional prefixes to
read, release-please has little to put under the version heading. The release body makes up for it — the build
appends GitHub's generated pull-request list underneath whatever the CHANGELOG said, so the notes stay at least as
full as they were before any of this was automated, and anything written by hand stays on top of them.

The pull request is the point. Until it is merged nothing is tagged, so a wrong version or a bad note is a comment
on a PR rather than a tag that has to be burned.

### The wrappers follow by themselves

The release build also opens a bump PR in each wrapper, authored by the cross-repo App (installed on exactly those
three repos): gocov-action's pins the CLI its `action.yml` installs; upload-pipe's bakes the CLI into the image and
bumps `pipe.yml` and the CHANGELOG; gitlab-component's bumps the `version` default in `templates/upload.yml`, the
README and the CHANGELOG. Each PR carries the `release` label, and merging it **is** that wrapper's release: a
`tag-on-release-merge` workflow tags the merge commit (the action and the component compute their next minor from
their tags; the pipe reads its version from `pipe.yml`) and runs that repo's release workflow — the action's release
moves `v1`, the pipe's builds the multi-arch image for Docker Hub, the component's mirrors the tag to gitlab.com,
where the project's own pipeline creates the release the CI/CD Catalog lists. So a full release across all four
repos is four PR merges and nothing else; between the gocov release and the wrapper merges, `verify-release` reports
the wrappers as behind, which is true.

The mirrors are the seams. The pipe's tag workflow pushes Bitbucket only when the
`BITBUCKET_MIRROR_USERNAME`/`BITBUCKET_MIRROR_APP_PASSWORD` secrets are set, the component's pushes gitlab.com only
when `GITLAB_MIRROR_TOKEN` is, and both warn instead of failing when they are not — the Atlassian catalog reads the
Bitbucket repo and the GitLab catalog reads the gitlab.com project, so verify-release checks the tags landed there.

### Where the version is written down

In the snippets a user copies — the CI recipes in [gitlab-ci](gitlab-ci.md) and [ci-other](ci-other.md), and the
onboarding wizard's template — plus the `PinnedCLIVersion` constant in `internal/hosted` that the wizard and its
test read. The copies cannot share that constant — some are Markdown, one is a Go template — so release-please
rewrites them all, guided by `x-release-please-start-version` markers (and a line marker on the constant). The
markers sit *outside* the snippets: an HTML comment in the Markdown, a Go template comment in the page, so neither
shows up in what a reader copies and neither reaches the browser.

Copies drift, and in August they did: the pipe image spent ten days baking a CLI two releases older than the one the
action installed, and nothing said so. Two scripts close that off from opposite ends.

`scripts/check-pins.sh` runs in CI and holds this repo's pins to *each other*. It says nothing about whether
that version is the latest, and cannot: the release PR is what bumps the pins, so between a release and its bump
every pin would look stale and main would be red for no reason. The wrapper repos have a script of the same name
doing the same job for their own copies.

`scripts/verify-release.sh` is the other end — it checks the published world after a release rather than the
working tree before one:

```sh
scripts/verify-release.sh            # the newest release
scripts/verify-release.sh v0.12.0    # a specific one
```

It confirms that the ten binaries and `checksums.txt` are on the release, that the checksums cover every binary and
that the release and the server image carry a build provenance attestation from this repository,
that this repo's snippets and all three wrappers name the released CLI, that `gocov-action@v1` resolves to the newest
action release, that the pipe image is on Docker Hub and the server image on GHCR for both architectures, and that
both images actually report the right version when opened. It also checks the pipe's tag reached Bitbucket and the
component's tag and release reached gitlab.com — those repos release through two remotes, and a tag that lands on
only one publishes nothing on the other silently. It needs
`gh`, `curl` and `jq`; `docker` is optional and only the last check needs it.

The `verify-release` workflow runs it weekly and on demand. It also runs on every published release, where it
reports rather than fails: at that moment the wrapper bumps are still open PRs, so the output is a to-do list, not a
verdict.

### If the bot is in the way

Turn it off and cut the tag by hand — `release.yml` still triggers on a pushed `v*` tag and still does the whole
job, creating the release itself when release-please has not already made one. Nothing about the release depends on
the bot being alive.
