package server

import (
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/store"
)

// Without a key the pages must carry no trace of PostHog: that is the
// "nothing off-site" promise self-hosted deployments rely on.
func TestPostHogOffLeavesPagesClean(t *testing.T) {
	f := newFixture(t, nil)
	body := get(f, "/").Body.String()
	if strings.Contains(body, "posthog") {
		t.Errorf("page mentions posthog with no key configured:\n%s", body)
	}
}

func TestPostHogMetaTag(t *testing.T) {
	f := newPublicFixture(t, store.VisibilityPublic, true)
	f.srv.posthog = PostHog{Key: "phc_abc", Host: "https://eu.i.posthog.com"}

	// Signed out (a public report page): key and host, no user.
	body := get(f, "/repos/acme/widgets").Body.String()
	want := `<meta name="gocov-posthog" content="phc_abc" data-host="https://eu.i.posthog.com">`
	if !strings.Contains(body, want) {
		t.Errorf("signed-out page missing %s:\n%s", want, body)
	}

	// Signed in: the numeric gocov id rides along, the email never does.
	sess := signIn(t, f, "/")
	body = get(f, "/", sess).Body.String()
	users, err := f.store.ListUsers(t.Context())
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers = %v, %v; want the one signed-in user", users, err)
	}
	want = `<meta name="gocov-posthog" content="phc_abc" data-host="https://eu.i.posthog.com" data-user="` +
		PostHog{}.view(users[0]).UserID + `">`
	if !strings.Contains(body, want) {
		t.Errorf("signed-in page missing %s:\n%s", want, body)
	}
	if strings.Contains(body, "jane@example.com") && strings.Contains(body, `data-user="jane`) {
		t.Error("meta tag carries the email")
	}
}
