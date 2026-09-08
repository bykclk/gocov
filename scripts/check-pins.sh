#!/usr/bin/env bash
# Every install snippet gocov hands a user pins an exact CLI release: the
# CI recipe docs (docs/gitlab-ci.md, docs/ci-other.md) and the onboarding
# wizard (internal/server/templates/onboarding.html) carry the same
# download URLs and the same `ver=` recipe, copied rather than shared,
# because some are Markdown and one is a Go template.
#
# Copies drift. This script fails CI when they stop agreeing with each
# other — the "updated the docs, forgot the wizard" mistake, which
# otherwise ships a stale snippet to exactly the people seeing gocov for
# the first time.
#
# Deliberately *not* checked here: whether that version is the newest
# release. It cannot be. The release commit bumps these pins, so between a
# release and its bump every pin would be "stale" and main would be red
# for no reason. Agreement with the released world is scripts/verify-
# release.sh's job, and it runs after the release, not before.
set -euo pipefail

cd "$(dirname "$0")/.."

# The forms a pin takes in a copyable snippet:
#   curl .../releases/download/v0.12.0/gocov-linux-amd64
#   ver=v0.12.0
#   image: ghcr.io/gocov/gocov-server:v0.12.0
#   GOCOV_VERSION=v0.12.0        (deploy/.env.example)
# (the server image and the CLI version together on every release, so one
# pin pool covers both)
# CHANGELOG.md is excluded: its older entries name older releases on
# purpose, and that is history, not drift. pinned_test.go is excluded
# because its doc comment quotes the download-URL shape with a literal
# version, and comments are not release-please's to bump.
pins=$(git grep -InEo \
  -e 'releases/download/v[0-9]+\.[0-9]+\.[0-9]+' \
  -e 'ver=v[0-9]+\.[0-9]+\.[0-9]+' \
  -e 'gocov-server:v[0-9]+\.[0-9]+\.[0-9]+' \
  -e 'GOCOV_VERSION=v[0-9]+\.[0-9]+\.[0-9]+' \
  -- ':!CHANGELOG.md' ':!scripts/check-pins.sh' ':!internal/hosted/pinned_test.go' || true)

if [ -z "$pins" ]; then
  echo "check-pins: no CLI version pins found at all." >&2
  echo "Either the snippets lost their pins, or this script's patterns went stale." >&2
  exit 1
fi

versions=$(echo "$pins" | grep -Eo 'v[0-9]+\.[0-9]+\.[0-9]+' | sort -u)
count=$(echo "$versions" | wc -l | tr -d ' ')

if [ "$count" -ne 1 ]; then
  echo "check-pins: the CLI version pins disagree." >&2
  echo >&2
  printf '%s\n' "$pins" | sed 's/^/  /' >&2
  echo >&2
  echo "Versions found: $(echo "$versions" | tr '\n' ' ')" >&2
  echo "Every snippet must name the same release. Fix the stragglers and re-run." >&2
  exit 1
fi

echo "check-pins: $(echo "$pins" | wc -l | tr -d ' ') pins, all at $versions"
