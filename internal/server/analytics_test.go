package server

import (
	"net/http"
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
	body := get(f, "/repos/bitbucket/acme/widgets").Body.String()
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

// The wizard declares its product events in the markup (data-ph-*), which
// static/app.js turns into PostHog captures when a key is configured. Pin
// the declarations so a template edit cannot silently drop a funnel step.
func TestOnboardingDeclaresProductEvents(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if body := get(f, "/login").Body.String(); !strings.Contains(body,
		`data-ph-click="sign_in_clicked" data-ph-forge="bitbucket"`) {
		t.Errorf("login page misses the sign-in click event:\n%s", body)
	}
	sess := hostedSignIn(t, f, "/", "/onboarding")
	body := get(f, "/onboarding", sess).Body.String()
	for _, want := range []string{
		`data-ph-view="onboarding_step_viewed" data-ph-step="workspace" data-ph-face="pick" data-ph-forge="bitbucket"`,
		`data-ph-click="register_workspace_clicked"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onboarding pick face misses %s:\n%s", want, body)
		}
	}
	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("register: status = %d", rec.Code)
	}
	body = get(f, "/onboarding?ws=acme", sess).Body.String()
	for _, want := range []string{
		`data-ph-step="workspace" data-ph-face="ready"`,
		`data-ph-click="continue_to_ci_clicked"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onboarding ready face misses %s:\n%s", want, body)
		}
	}
	body = get(f, "/workspaces/bitbucket/acme/setup", sess).Body.String()
	for _, want := range []string{
		`data-ph-step="wire_ci" data-ph-face="ci"`,
		`data-ph-click="reveal_token_clicked"`, `data-ph-click="copy_token_clicked"`,
		`data-ph-click="copy_snippet_clicked"`, `data-ph-click="added_to_ci_clicked"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("CI step misses %s:\n%s", want, body)
		}
	}
	if body := get(f, "/workspaces/bitbucket/acme/setup?awaiting=1", sess).Body.String(); !strings.Contains(body,
		`data-ph-step="first_upload" data-ph-face="awaiting"`) {
		t.Errorf("waiting face misses its step context:\n%s", body)
	}

	// The poll redirects to the setup page with ?landed=1 when the first
	// upload arrives; only that load reports first_upload_received, while
	// every load in that state reports the received face.
	ws, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if rec := doUpload(t, f, ws.Token, map[string]string{
		"repo": "acme/newrepo", "commit": "c1", "branch": "main"}, testProfile); rec.Code != http.StatusCreated {
		t.Fatalf("upload: status = %d: %s", rec.Code, rec.Body)
	}
	if loc := get(f, "/workspaces/bitbucket/acme/setup/status", sess).Header().Get("HX-Redirect"); loc != "/workspaces/bitbucket/acme/setup?landed=1" {
		t.Errorf("status poll redirected to %q, want the setup page marked landed=1", loc)
	}
	landed := get(f, "/workspaces/bitbucket/acme/setup?landed=1", sess).Body.String()
	for _, want := range []string{
		`data-ph-step="first_upload" data-ph-face="received"`,
		`data-ph-view="first_upload_received"`, `data-ph-click="open_dashboard_clicked"`,
	} {
		if !strings.Contains(landed, want) {
			t.Errorf("landed page misses %s:\n%s", want, landed)
		}
	}
	if body := get(f, "/workspaces/bitbucket/acme/setup", sess).Body.String(); strings.Contains(body, "first_upload_received") ||
		!strings.Contains(body, `data-ph-face="received"`) {
		t.Errorf("a plain reload of the received state must keep the face but not re-report the landing:\n%s", body)
	}
}
