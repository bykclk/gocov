package hosted

import (
	"os"
	"regexp"
	"testing"
)

// pinnedIn are the files whose copy-paste snippets install a specific CLI
// release. All are read by users following instructions, so a stale
// version here is a broken pipeline for someone, not a typo.
var pinnedIn = []string{
	"../../docs/gitlab-ci.md",
	"../../docs/ci-other.md",
	"../../docs/self-hosting.md",
	"../../internal/server/templates/onboarding.html",
	"../../deploy/.env.example",
}

// version matches the pinned release in a download URL, a shell assignment,
// the server image reference or the compose .env — "releases/download/…",
// "ver=…", "gocov-server:…" and "GOCOV_VERSION=…" — which is every shape
// the snippets currently use to name one. The server image versions with
// the CLI (same repo, same tag), so one constant covers both.
var version = regexp.MustCompile(`(?:releases/download/|\bver=|gocov-server:|GOCOV_VERSION=)(v\d+\.\d+\.\d+)`)

// TestPinnedCLIVersionIsInSync keeps the snippets and PinnedCLIVersion from
// drifting apart. Releasing bumps the constant; this fails until every
// snippet has followed.
func TestPinnedCLIVersionIsInSync(t *testing.T) {
	for _, path := range pinnedIn {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		found := version.FindAllStringSubmatch(string(body), -1)
		if len(found) == 0 {
			t.Errorf("%s: no pinned CLI version found — did the snippets change shape?", path)
			continue
		}
		for _, m := range found {
			if m[1] != PinnedCLIVersion {
				t.Errorf("%s: snippet pins %s, PinnedCLIVersion is %s", path, m[1], PinnedCLIVersion)
			}
		}
	}
}
