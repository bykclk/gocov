# Other CI systems

gocov is not tied to any CI. Any environment that can run a shell command can upload: run your tests, download the
CLI, `gocov upload` the report. Outside the CIs the CLI knows, repo, commit and branch fall back to `git`, or can be
passed as [flags](cli.md).

Set `GOCOV_TOKEN` in the environment (your workspace's [upload token](getting-started.md), as a secret) and, when
self-hosting, `GOCOV_SERVER`.

## Prebuilt binaries

Every release ships static binaries for linux, darwin and windows on amd64 and arm64, with `checksums.txt` alongside —
no toolchain needed. Pin a version, and cache the download on self-hosted runners:

<!-- x-release-please-start-version -->
```sh
ver=v0.22.0
arch=$(uname -m); case "$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac
bin="$HOME/.cache/gocov/gocov-$ver-linux-$arch"
if [ ! -x "$bin" ]; then
  mkdir -p "$(dirname "$bin")"
  curl -fsSL "https://github.com/gocov/gocov/releases/download/$ver/gocov-linux-$arch" -o "$bin"
  chmod +x "$bin"
fi
"$bin" upload coverage.out
```
<!-- x-release-please-end -->

## With a Go toolchain

On a runner that already has Go, skip the download:

```sh
go run github.com/gocov/gocov/cmd/gocov@latest upload coverage.out
```

## Outside CI

Anything not detected can be passed explicitly:

```sh
gocov upload -token $TOKEN \
  -repo myworkspace/myrepo -commit $(git rev-parse HEAD) -branch main \
  coverage.out
```

(Add `-server https://gocov.example` when self-hosting.) The full flag list is in the [CLI reference](cli.md).
