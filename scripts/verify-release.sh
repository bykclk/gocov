#!/usr/bin/env bash
# What actually shipped? One command, four repos, two registries, two
# catalogs.
#
# A gocov release is not finished when the tag is cut. The CLI's binaries
# have to be on the release, the action has to pin the new CLI and its
# floating v1 has to point at the new action, the pipe image has to be on
# Docker Hub for both architectures with the new CLI baked in, the pipe's
# tag has to exist on *both* of its remotes, and the GitLab component has
# to pin the new CLI, reach its gitlab.com mirror and be released to the
# CI/CD Catalog there. Checked by hand until now — which is how the pipe
# spent ten days in August shipping a CLI two releases old without anyone
# noticing.
#
# Every check reads the published world over the network: GitHub releases
# and tags, the default branches, the Docker registry, Bitbucket's and
# GitLab's tag lists. Nothing here trusts the working tree, because the
# working tree is not what users get.
#
# Usage:
#   scripts/verify-release.sh            # verify the newest gocov release
#   scripts/verify-release.sh v0.12.0    # verify a specific one
#
# Needs: gh (authenticated), curl, jq. docker is optional — without it the
# one check that runs the published image is skipped, loudly.
#
# Exits non-zero if any check failed, so it can gate a workflow. Checks do
# not stop at the first failure: after a release you want the whole
# picture, not the first thing that went wrong.
set -uo pipefail

CLI_REPO=gocov/gocov
ACTION_REPO=gocov/gocov-action
PIPE_REPO=gocov/upload-pipe
PIPE_IMAGE=gocov/upload-pipe
PIPE_BITBUCKET=gocov/upload-pipe
SERVER_IMAGE=gocov/gocov-server
COMPONENT_REPO=gocov/gitlab-component
COMPONENT_GITLAB=gocov/gocov   # the mirror, where the CI/CD Catalog entry lives
# The first release whose workflow attested its assets and image. Older
# tags legitimately have no provenance, so the check is skipped for them
# rather than failing a scheduled run that verifies an older release.
FIRST_ATTESTED=v0.24.0

pass=0 fail=0 skipped=0

ok()   { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %s\n' "$1"; shift; [ $# -gt 0 ] && printf '        %s\n' "$@"; fail=$((fail + 1)); }
skip() { printf '  skip  %s\n' "$1"; shift; [ $# -gt 0 ] && printf '        %s\n' "$@"; skipped=$((skipped + 1)); }
head2() { printf '\n%s\n' "$1"; }

for tool in gh curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "verify-release: $tool is required but not installed" >&2; exit 2; }
done

# The version under test: the argument, or whatever gocov released last.
tag=${1:-}
if [ -z "$tag" ]; then
  tag=$(gh release view --repo "$CLI_REPO" --json tagName --jq .tagName 2>/dev/null)
  [ -n "$tag" ] || { echo "verify-release: could not read the latest $CLI_REPO release" >&2; exit 2; }
fi

echo "verify-release: checking gocov $tag across four repos"

# Reads one file from a repo at a ref. Prints nothing and returns 1 when
# the file or the ref is missing, so callers can report that themselves.
contents() {
  gh api "repos/$1/contents/$2?ref=$3" --jq .content 2>/dev/null | base64 -d 2>/dev/null
}

# ---------------------------------------------------------------- CLI --
head2 "$CLI_REPO @ $tag"

assets=$(gh release view "$tag" --repo "$CLI_REPO" --json assets --jq '.assets[].name' 2>/dev/null | sort)
if [ -z "$assets" ]; then
  bad "release $tag has no assets, or does not exist"
else
  want=""
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
    os=${target%/*}; arch=${target#*/}
    ext=""; [ "$os" = "windows" ] && ext=".exe"
    want="$want gocov-$os-$arch$ext gocov-server-$os-$arch$ext"
  done
  want="$want checksums.txt"
  missing=""
  for a in $want; do
    echo "$assets" | grep -qxF "$a" || missing="$missing $a"
  done
  if [ -n "$missing" ]; then
    bad "release assets incomplete" "missing:$missing"
  else
    ok "all 10 binaries and checksums.txt published"
  fi

  # A checksums.txt that does not cover every binary is worse than none:
  # the action and the pipe both verify against it and would fail closed
  # at install time, on the user's runner rather than here.
  sums=$(curl -fsSL "https://github.com/$CLI_REPO/releases/download/$tag/checksums.txt" 2>/dev/null)
  if [ -z "$sums" ]; then
    bad "checksums.txt could not be downloaded"
  else
    unsummed=""
    for a in $want; do
      [ "$a" = checksums.txt ] && continue
      echo "$sums" | grep -qE "[[:space:]]\*?$a\$" || unsummed="$unsummed $a"
    done
    if [ -n "$unsummed" ]; then
      bad "checksums.txt does not cover every binary" "missing:$unsummed"
    else
      ok "checksums.txt covers every binary"
    fi

    # Build provenance: checksums.txt is attested along with every binary,
    # and it is the one file the action and the pipe trust at install
    # time, so verifying it here covers the download path they take.
    if [ "$(printf '%s\n%s\n' "$FIRST_ATTESTED" "$tag" | sort -V | head -1)" != "$FIRST_ATTESTED" ]; then
      skip "did not check build provenance" "releases before $FIRST_ATTESTED were not attested"
    else
      tmp=$(mktemp -d); printf '%s\n' "$sums" >"$tmp/checksums.txt"
      if gh attestation verify "$tmp/checksums.txt" --repo "$CLI_REPO" >/dev/null 2>&1; then
        ok "checksums.txt carries build provenance from $CLI_REPO"
      else
        bad "checksums.txt has no valid build provenance from $CLI_REPO" \
          "the release job's attest step did not run, or attested something else"
      fi
      rm -rf "$tmp"
    fi
  fi
fi

# The docs and the onboarding wizard tell users which release to download.
# check-pins.sh keeps those pins in step with each other; only here, after
# the release exists, can they be held to the release itself.
docs_stale=""
for path in docs/gitlab-ci.md docs/ci-other.md docs/self-hosting.md internal/server/templates/onboarding.html; do
  body=$(contents "$CLI_REPO" "$path" main)
  if [ -z "$body" ]; then
    docs_stale="$docs_stale $path(unreadable)"
    continue
  fi
  for v in $(echo "$body" | grep -Eo -e 'releases/download/v[0-9]+\.[0-9]+\.[0-9]+' -e 'ver=v[0-9]+\.[0-9]+\.[0-9]+' -e 'gocov-server:v[0-9]+\.[0-9]+\.[0-9]+' |
    grep -Eo 'v[0-9]+\.[0-9]+\.[0-9]+' | sort -u); do
    [ "$v" = "$tag" ] || docs_stale="$docs_stale $path:$v"
  done
done
if [ -n "$docs_stale" ]; then
  bad "install snippets on main still name an older release" "$docs_stale" "expected $tag"
else
  ok "install snippets on main all name $tag"
fi

# ------------------------------------------------------------- action --
head2 "$ACTION_REPO"

action_tag=$(gh release view --repo "$ACTION_REPO" --json tagName --jq .tagName 2>/dev/null)
if [ -z "$action_tag" ]; then
  bad "could not read the latest $ACTION_REPO release"
else
  ok "latest action release is $action_tag"

  released_pin=$(contents "$ACTION_REPO" action.yml "$action_tag" |
    sed -n 's/^ *default: *\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$released_pin" = "$tag" ]; then
    ok "action $action_tag pins CLI $tag"
  else
    bad "action $action_tag pins CLI ${released_pin:-<none found>}, not $tag" \
      "users on gocov-action@v1 are getting ${released_pin:-an unknown CLI}"
  fi

  main_pin=$(contents "$ACTION_REPO" action.yml main |
    sed -n 's/^ *default: *\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$main_pin" = "$tag" ]; then
    ok "action main pins CLI $tag"
  else
    bad "action main pins CLI ${main_pin:-<none found>}, not $tag" "the bump PR may be unmerged"
  fi

  # `uses: gocov/gocov-action@v1` resolves through the floating major tag,
  # so a release that did not move it reaches nobody.
  v1=$(gh api "repos/$ACTION_REPO/commits/v1" --jq .sha 2>/dev/null)
  at=$(gh api "repos/$ACTION_REPO/commits/$action_tag" --jq .sha 2>/dev/null)
  if [ -z "$v1" ] || [ -z "$at" ]; then
    bad "could not resolve the v1 or $action_tag tag"
  elif [ "$v1" = "$at" ]; then
    ok "v1 points at $action_tag"
  else
    bad "v1 does not point at $action_tag" "v1=$v1" "$action_tag=$at"
  fi
fi

# --------------------------------------------------------------- pipe --
head2 "$PIPE_REPO"

pipe_tag=$(gh api "repos/$PIPE_REPO/tags" --jq '.[].name' 2>/dev/null |
  grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)
if [ -z "$pipe_tag" ]; then
  bad "could not read $PIPE_REPO tags"
else
  ok "latest pipe tag is $pipe_tag"

  released_arg=$(contents "$PIPE_REPO" Dockerfile "$pipe_tag" |
    sed -n 's/^ARG GOCOV_VERSION=\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$released_arg" = "$tag" ]; then
    ok "pipe $pipe_tag bakes CLI $tag"
  else
    bad "pipe $pipe_tag bakes CLI ${released_arg:-<none found>}, not $tag" \
      "this is exactly the August drift: Bitbucket users get the old CLI"
  fi

  main_arg=$(contents "$PIPE_REPO" Dockerfile main |
    sed -n 's/^ARG GOCOV_VERSION=\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$main_arg" = "$tag" ]; then
    ok "pipe main bakes CLI $tag"
  else
    bad "pipe main bakes CLI ${main_arg:-<none found>}, not $tag" "the bump PR may be unmerged"
  fi

  # The pipe is released by pushing its tag to two remotes. A tag that
  # reached only one of them publishes nothing on the other, silently.
  bb=$(curl -fsSL "https://api.bitbucket.org/2.0/repositories/$PIPE_BITBUCKET/refs/tags?pagelen=100" 2>/dev/null |
    jq -r '.values[]?.name' 2>/dev/null)
  if [ -z "$bb" ]; then
    skip "could not read Bitbucket tags for $PIPE_BITBUCKET" "the mirror check needs api.bitbucket.org to be reachable"
  elif echo "$bb" | grep -qxF "$pipe_tag"; then
    ok "tag $pipe_tag is on Bitbucket as well as GitHub"
  else
    bad "tag $pipe_tag is on GitHub but not on Bitbucket" \
      "the Atlassian pipe catalog reads the Bitbucket repo; push the tag there too"
  fi
fi

# ----------------------------------------------------------- registry --
head2 "docker.io/$PIPE_IMAGE"

reg_token=$(curl -fsSL "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$PIPE_IMAGE:pull" 2>/dev/null | jq -r .token 2>/dev/null)
if [ -z "$reg_token" ] || [ "$reg_token" = null ]; then
  bad "could not get a Docker Hub pull token for $PIPE_IMAGE"
else
  # `pipe: docker://gocov/upload-pipe:0` is what users write, so :0 is the
  # tag that matters — an image pushed only as :X.Y.Z reaches no one.
  for ref in "0" "${pipe_tag:-}"; do
    [ -n "$ref" ] || continue
    arches=$(curl -fsSL -H "Authorization: Bearer $reg_token" \
      -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
      "https://registry-1.docker.io/v2/$PIPE_IMAGE/manifests/$ref" 2>/dev/null |
      jq -r '.manifests[]? | "\(.platform.os)/\(.platform.architecture)"' 2>/dev/null | sort -u | tr '\n' ' ')
    if echo "$arches" | grep -q 'linux/amd64' && echo "$arches" | grep -q 'linux/arm64'; then
      ok ":$ref is a multi-arch image (${arches% })"
    else
      bad ":$ref is not published for both architectures" "found: ${arches:-nothing}"
    fi
  done
fi

# The claim this whole release turns on: the image users actually pull
# contains the CLI this release published. Everything above is metadata
# agreeing with metadata; this one opens the box.
if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; then
  baked=$(docker run --rm --pull always --entrypoint gocov "$PIPE_IMAGE:0" version 2>/dev/null | tr -d '\r' | awk '{print $NF}')
  if [ -z "$baked" ]; then
    bad "could not run gocov inside $PIPE_IMAGE:0"
  elif [ "$baked" = "$tag" ]; then
    ok "$PIPE_IMAGE:0 reports gocov $baked"
  else
    bad "$PIPE_IMAGE:0 reports gocov $baked, not $tag"
  fi
else
  skip "did not open $PIPE_IMAGE:0 to check the baked CLI" "docker is not available here"
fi

# ---------------------------------------------------------- component --
head2 "$COMPONENT_REPO"

# The GitLab component's release lives in two places: the GitHub repo
# that is its source, and the gitlab.com mirror whose pipeline creates the
# release the CI/CD Catalog lists. A tag on only one is a component that
# says it pins the new CLI while `include: component: …@1` keeps serving
# the old one.
comp_tag=$(gh release view --repo "$COMPONENT_REPO" --json tagName --jq .tagName 2>/dev/null)
if [ -z "$comp_tag" ]; then
  bad "could not read the latest $COMPONENT_REPO release"
else
  ok "latest component release is $comp_tag"

  released_pin=$(contents "$COMPONENT_REPO" templates/upload.yml "$comp_tag" |
    sed -n 's/^ *default: *\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$released_pin" = "$tag" ]; then
    ok "component $comp_tag installs CLI $tag"
  else
    bad "component $comp_tag installs CLI ${released_pin:-<none found>}, not $tag" \
      "users on upload@1 are getting ${released_pin:-an unknown CLI}"
  fi

  main_pin=$(contents "$COMPONENT_REPO" templates/upload.yml main |
    sed -n 's/^ *default: *\(v[0-9][0-9.]*\) *$/\1/p')
  if [ "$main_pin" = "$tag" ]; then
    ok "component main installs CLI $tag"
  else
    bad "component main installs CLI ${main_pin:-<none found>}, not $tag" "the bump PR may be unmerged"
  fi

  gl_project="https://gitlab.com/api/v4/projects/$(printf %s "$COMPONENT_GITLAB" | sed 's|/|%2F|g')"
  if curl -fsS "$gl_project/repository/tags/$comp_tag" >/dev/null 2>&1; then
    ok "tag $comp_tag is on gitlab.com as well as GitHub"
  else
    bad "tag $comp_tag is not on gitlab.com/$COMPONENT_GITLAB" \
      "the mirror push did not happen; the catalog cannot see this release"
  fi
  if curl -fsS "$gl_project/releases/$comp_tag" >/dev/null 2>&1; then
    ok "$comp_tag is released on gitlab.com (what the CI/CD Catalog lists)"
  else
    bad "gitlab.com/$COMPONENT_GITLAB has no release $comp_tag" \
      "the mirror pipeline's release job did not run or failed"
  fi
fi

# ------------------------------------------------------ server image --
head2 "ghcr.io/$SERVER_IMAGE"

# The server image is what production deploys and what self-hosters pull;
# both pin the exact release tag, so a tag that is missing or half-built
# is a failed deploy waiting for its approval click.
ghcr_token=$(curl -fsSL "https://ghcr.io/token?scope=repository:$SERVER_IMAGE:pull" 2>/dev/null | jq -r .token 2>/dev/null)
if [ -z "$ghcr_token" ] || [ "$ghcr_token" = null ]; then
  bad "could not get a GHCR pull token for $SERVER_IMAGE" "is the package public?"
else
  # architecture "unknown" entries are buildx attestation manifests, not
  # runnable platforms.
  arches=$(curl -fsSL -H "Authorization: Bearer $ghcr_token" \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://ghcr.io/v2/$SERVER_IMAGE/manifests/$tag" 2>/dev/null |
    jq -r '.manifests[]? | select(.platform.architecture != "unknown") | "\(.platform.os)/\(.platform.architecture)"' 2>/dev/null | sort -u | tr '\n' ' ')
  if echo "$arches" | grep -q 'linux/amd64' && echo "$arches" | grep -q 'linux/arm64'; then
    ok ":$tag is a multi-arch image (${arches% })"
  else
    bad ":$tag is not published for both architectures" "found: ${arches:-nothing}" \
      "production (arm64) and most self-hosters (amd64) pull this by exact version"
  fi

  # The image's provenance lives in the registry next to the manifest;
  # gh resolves the tag to its digest and looks it up there.
  if [ "$(printf '%s\n%s\n' "$FIRST_ATTESTED" "$tag" | sort -V | head -1)" != "$FIRST_ATTESTED" ]; then
    skip "did not check the image's build provenance" "releases before $FIRST_ATTESTED were not attested"
  elif gh attestation verify "oci://ghcr.io/$SERVER_IMAGE:$tag" --repo "$CLI_REPO" >/dev/null 2>&1; then
    ok "ghcr.io/$SERVER_IMAGE:$tag carries build provenance from $CLI_REPO"
  else
    bad "ghcr.io/$SERVER_IMAGE:$tag has no valid build provenance from $CLI_REPO" \
      "the image job's attest step did not run, or was not pushed to the registry"
  fi
fi

# Same open-the-box check as the pipe: the version the image reports is
# the one the tag promises.
if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; then
  reported=$(docker run --rm --pull always "ghcr.io/$SERVER_IMAGE:$tag" version 2>/dev/null | tr -d '\r' | awk '{print $NF}')
  if [ -z "$reported" ]; then
    bad "could not run gocov-server inside ghcr.io/$SERVER_IMAGE:$tag"
  elif [ "$reported" = "$tag" ]; then
    ok "ghcr.io/$SERVER_IMAGE:$tag reports $reported"
  else
    bad "ghcr.io/$SERVER_IMAGE:$tag reports $reported, not $tag"
  fi
else
  skip "did not open ghcr.io/$SERVER_IMAGE:$tag to check its version" "docker is not available here"
fi

# -------------------------------------------------------------- verdict
printf '\n%s\n' "$pass passed, $fail failed${skipped:+, $skipped skipped}"
if [ "$fail" -gt 0 ]; then
  echo "gocov $tag is NOT fully released."
  exit 1
fi
echo "gocov $tag is released everywhere it needs to be."
