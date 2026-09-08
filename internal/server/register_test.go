package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/auth"
	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// newHostedFixture builds a hosted-mode server (M3/D1) with an empty
// store: no tracked workspaces, like a fresh SaaS instance.
func newHostedFixture(t *testing.T, provider auth.Provider) *fixture {
	t.Helper()
	st := storemem.New()
	srv := New(Config{
		Store:   st,
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		BaseURL: "https://gocov.example",
		Auths:   []auth.Provider{provider},
		Hosted:  true,
	})
	return &fixture{srv: srv, store: st}
}

// hostedSignIn drives the OAuth flow and asserts the callback lands on
// wantNext, returning the session cookie.
func hostedSignIn(t *testing.T, f *fixture, next, wantNext string) *http.Cookie {
	t.Helper()
	start := get(f, "/oauth/bitbucket/start?next="+url.QueryEscape(next))
	if start.Code != http.StatusFound {
		t.Fatalf("start: status = %d", start.Code)
	}
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")
	cb := get(f, "/oauth/bitbucket/callback?code=thecode&state="+url.QueryEscape(state), stateCk)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback: status = %d", cb.Code)
	}
	if loc := cb.Header().Get("Location"); loc != wantNext {
		t.Fatalf("callback redirected to %q, want %q", loc, wantNext)
	}
	return cookieNamed(t, cb, sessionCookie)
}

func postRegister(f *fixture, prefix string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/register",
		strings.NewReader(url.Values{"prefix": {prefix}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestHostedAdmitsNonMemberAndRoutesToRegistration(t *testing.T) {
	// This identity would be denied on a private instance (no tracked
	// workspace matches); hosted mode signs it in and lands on /register.
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})

	sess := hostedSignIn(t, f, "/", "/onboarding")

	// The user exists with the login-time workspace snapshot (D3).
	users, err := f.store.ListUsers(t.Context())
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	if !reflect.DeepEqual(users[0].ForgeWorkspaces, []string{"acme", "personal"}) {
		t.Errorf("stored forge workspaces = %v", users[0].ForgeWorkspaces)
	}

	// The dashboard bounces a membership-less hosted user to onboarding.
	if rec := get(f, "/", sess); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/onboarding" {
		t.Errorf("index: %d -> %q, want redirect to /onboarding", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHostedUserWithMembershipLandsOnIndex(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	sess := hostedSignIn(t, f, "/", "/")
	if rec := get(f, "/", sess); rec.Code != http.StatusOK {
		t.Errorf("index for member: status = %d", rec.Code)
	}
}

func TestRegistrationWorksInPrivateMode(t *testing.T) {
	// A signed-in private-mode user can register a workspace their forge
	// vouches for — the bootstrap path now that the admin CLI is gone. The
	// signed-in identity is a member of "acme" and "personal" (memberIdentity).
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)
	sess := signIn(t, f, "/")

	// GET /register preserves the old URL by redirecting to the wizard.
	if rec := get(f, "/register", sess); rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/onboarding" {
		t.Errorf("GET /register: status = %d, location = %q; want 302 -> /onboarding",
			rec.Code, rec.Header().Get("Location"))
	}
	// Claiming a forge-vouched workspace creates it and lands on the ready state.
	rec := postRegister(f, "personal", sess)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /register: status = %d, body = %s", rec.Code, rec.Body)
	}
	if _, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "personal"); err != nil {
		t.Fatalf("workspace not registered: %v", err)
	}
	// The forge-list check still gates: a workspace the identity does not list.
	if rec := postRegister(f, "notmine", sess); rec.Code != http.StatusForbidden {
		t.Errorf("POST /register for an unvouched workspace: status = %d, want 403", rec.Code)
	}
}

func TestRegistrationNeedsSignIn(t *testing.T) {
	// An open instance (no sign-in provider) has no forge identity to
	// register from, so the registration routes stay 404.
	f := newAuthFixture(t, nil, nil)
	if rec := get(f, "/onboarding"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /onboarding with auth disabled: status = %d, want 404", rec.Code)
	}
	if rec := get(f, "/register"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /register with auth disabled: status = %d, want 404", rec.Code)
	}
}

// TestHostedReauthHonorsPendingInstall locks the callback change behind the
// GitHub install self-heal: a re-auth aimed at a pending install must reach
// /github/setup even for a zero-membership user, instead of being forced to
// /onboarding, so it can connect once the org snapshot refreshes.
func TestHostedReauthHonorsPendingInstall(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	next := "/github/setup?installation_id=5"
	hostedSignIn(t, f, next, next) // asserts the callback honours next verbatim
}

func TestRegisterPageStates(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	ctx := t.Context()
	// "personal" is free, and so is "acme": the GitHub workspace of that
	// name is another forge's tenant and says nothing about Bitbucket's.
	if err := f.store.CreateWorkspace(ctx,
		&store.Workspace{Forge: "github", Prefix: "acme", Token: "gh-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	sess := hostedSignIn(t, f, "/", "/onboarding")
	rec := get(f, "/onboarding", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("onboarding picker: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "personal") || !strings.Contains(body, "Not set up") {
		t.Errorf("free workspace not offered:\n%s", body)
	}
	if strings.Contains(body, "registered under") || strings.Count(body, "Create the workspace") != 2 {
		t.Errorf("a same-named workspace on another forge must not block the name:\n%s", body)
	}
	// "Sign in again" re-runs OAuth to refresh the snapshot. It must not
	// point at /logout, which is POST-only and 404s on a link's GET.
	if strings.Contains(body, `href="/logout"`) {
		t.Error("pick page links the POST-only /logout, which 404s on GET")
	}
	if !strings.Contains(body, "/start?next=/onboarding") {
		t.Errorf("pick page misses the re-auth 'sign in again' link:\n%s", body)
	}
}

func TestRegisterCreatesWorkspaceAndShowsTokenOnce(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	sess := hostedSignIn(t, f, "/", "/onboarding")

	rec := postRegister(f, "personal", sess)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/onboarding?ws=personal" {
		t.Fatalf("register: %d -> %q, want redirect to the workspace-ready state", rec.Code, rec.Header().Get("Location"))
	}

	ctx := t.Context()
	ws, err := f.store.WorkspaceByPrefix(ctx, "bitbucket", "personal")
	if err != nil {
		t.Fatalf("workspace not created: %v", err)
	}
	if ws.Forge != "bitbucket" || ws.Token == "" || ws.DefaultBranch != "main" {
		t.Errorf("workspace = %+v", ws)
	}
	// The onboarding page shows the token in the CI snippet (D6).
	setup := get(f, "/workspaces/bitbucket/personal/setup", sess)
	if setup.Code != http.StatusOK || !strings.Contains(setup.Body.String(), ws.Token) {
		t.Errorf("setup page (status %d) does not show the upload token:\n%s", setup.Code, setup.Body)
	}

	// Registration made the user a member atomically (R2), as the owner...
	users, _ := f.store.ListUsers(ctx)
	wss, err := f.store.ListWorkspacesForUser(ctx, users[0].ID)
	if err != nil || len(wss) != 1 || wss[0].Prefix != "personal" {
		t.Fatalf("memberships after registration = %v, %v", wss, err)
	}
	if got := membershipRoles(t, f, users[0].ID); !reflect.DeepEqual(got, map[string]store.Role{"personal": store.RoleOwner}) {
		t.Errorf("roles after registration = %v, want the founder as owner", got)
	}
	// ...so the dashboard opens instead of bouncing back to /register.
	if rec := get(f, "/", sess); rec.Code != http.StatusOK {
		t.Errorf("index after registration: status = %d", rec.Code)
	}

	// The onboarding picker now shows the workspace as joined, without the token.
	page := get(f, "/onboarding", sess)
	if !strings.Contains(page.Body.String(), "member") {
		t.Errorf("onboarding picker misses the member state:\n%s", page.Body)
	}
	if strings.Contains(page.Body.String(), ws.Token) {
		t.Error("onboarding picker leaks the upload token")
	}
}

func TestRegisterRejectsForeignPrefix(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	sess := hostedSignIn(t, f, "/", "/onboarding")

	// "evilcorp" is not in the identity's forge workspace list; the form
	// value must be rejected server-side regardless of what was posted.
	for _, prefix := range []string{"evilcorp", ""} {
		if rec := postRegister(f, prefix, sess); rec.Code != http.StatusForbidden {
			t.Errorf("register %q: status = %d, want 403", prefix, rec.Code)
		}
	}
	if _, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "evilcorp"); err == nil {
		t.Error("rejected registration created a workspace")
	}
}

func TestRegisterSameNameOnAnotherForgeIsItsOwnWorkspace(t *testing.T) {
	// Names are scoped per forge: the GitHub org "acme" and the Bitbucket
	// workspace "acme" are two tenants, so the Bitbucket user creates
	// theirs beside the GitHub one rather than being turned away.
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	gh := &store.Workspace{Forge: "github", Prefix: "acme", Token: "gh-tok", DefaultBranch: "main"}
	if err := f.store.CreateWorkspace(t.Context(), gh); err != nil {
		t.Fatal(err)
	}
	sess := hostedSignIn(t, f, "/", "/onboarding")

	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusSeeOther {
		t.Fatalf("claim beside another forge's namesake: status = %d, want 303", rec.Code)
	}
	bb, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "acme")
	if err != nil {
		t.Fatalf("bitbucket acme not registered: %v", err)
	}
	if bb.ID == gh.ID || bb.Token == gh.Token {
		t.Error("the claim joined the GitHub workspace instead of creating a Bitbucket one")
	}
}

func TestRegisterAlreadyRegisteredJoins(t *testing.T) {
	// A colleague registered "acme" after this user's login sync; claiming
	// it is a non-event (D2): membership is granted, no new workspace.
	// Joining is open to plain members — only creating takes an owner.
	f := newHostedFixture(t, &fakeProvider{identity: plainMemberIdentity()})
	sess := hostedSignIn(t, f, "/", "/onboarding")
	ctx := t.Context()
	if err := f.store.CreateWorkspace(ctx,
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	rec := postRegister(f, "acme", sess)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("join: %d -> %q, want redirect to /", rec.Code, rec.Header().Get("Location"))
	}
	users, _ := f.store.ListUsers(ctx)
	wss, err := f.store.ListWorkspacesForUser(ctx, users[0].ID)
	if err != nil || len(wss) != 1 || wss[0].Prefix != "acme" {
		t.Errorf("memberships after join = %v, %v", wss, err)
	}
	// The forge snapshot says plain member, so that is the seat granted.
	if got := membershipRoles(t, f, users[0].ID); !reflect.DeepEqual(got, map[string]store.Role{"acme": store.RoleMember}) {
		t.Errorf("roles after join = %v, want member", got)
	}
	// Idempotent: joining again keeps the single membership.
	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusFound {
		t.Errorf("second join: status = %d", rec.Code)
	}
	if wss, _ := f.store.ListWorkspacesForUser(ctx, users[0].ID); len(wss) != 1 {
		t.Errorf("second join changed memberships: %v", wss)
	}
}

func TestRegisterNeedsAnOwnerOnTheForge(t *testing.T) {
	// Creating a workspace makes a tenant and mints its upload token, both
	// owner-only from then on — so a forge member who is not an admin of
	// the workspace cannot be the one to create it. The picker says so
	// instead of offering the button, and the POST is refused regardless.
	f := newHostedFixture(t, &fakeProvider{identity: plainMemberIdentity()})
	sess := hostedSignIn(t, f, "/", "/onboarding")

	body := get(f, "/onboarding", sess).Body.String()
	if !strings.Contains(body, "Owners only") || strings.Contains(body, "Create the workspace") {
		t.Errorf("picker must mark an unowned workspace instead of offering to create it:\n%s", body)
	}
	if rec := postRegister(f, "personal", sess); rec.Code != http.StatusForbidden {
		t.Errorf("member creating a workspace: status = %d, want 403", rec.Code)
	}
	if _, err := f.store.WorkspaceByPrefix(t.Context(), "bitbucket", "personal"); err == nil {
		t.Error("refused registration created a workspace")
	}
}

func TestJoinTakesTheForgeRole(t *testing.T) {
	// Same join, but the forge says the account administers acme: the
	// seat granted is an owner's, exactly what the next login sync gives.
	identity := memberIdentity()
	identity.OwnedWorkspaces = []string{"acme"}
	f := newHostedFixture(t, &fakeProvider{identity: identity})
	sess := hostedSignIn(t, f, "/", "/onboarding")
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusFound {
		t.Fatalf("join: status = %d", rec.Code)
	}
	users, _ := f.store.ListUsers(t.Context())
	if got := membershipRoles(t, f, users[0].ID); !reflect.DeepEqual(got, map[string]store.Role{"acme": store.RoleOwner}) {
		t.Errorf("roles after join = %v, want owner", got)
	}
}

func TestReLoginRefreshesForgeWorkspaces(t *testing.T) {
	provider := &fakeProvider{identity: memberIdentity()}
	f := newHostedFixture(t, provider)
	hostedSignIn(t, f, "/", "/onboarding")

	provider.identity.Workspaces = []string{"acme", "personal", "newco"}
	provider.identity.OwnedWorkspaces = []string{"newco"}
	hostedSignIn(t, f, "/", "/onboarding")

	users, _ := f.store.ListUsers(t.Context())
	if len(users) != 1 || !reflect.DeepEqual(users[0].ForgeWorkspaces, []string{"acme", "personal", "newco"}) {
		t.Errorf("stored list not refreshed: %+v", users)
	}
	if len(users) != 1 || !reflect.DeepEqual(users[0].ForgeOwnedWorkspaces, []string{"newco"}) {
		t.Errorf("stored owned list not refreshed: %+v", users)
	}
}

func TestLoginSyncsRolesFromTheForge(t *testing.T) {
	// Roles mirror the forge like membership does: an admin there is an
	// owner here, and the next sign-in re-syncs both directions —
	// promotion and demotion — without touching the membership itself.
	identity := memberIdentity() // acme and personal, owner of neither
	provider := &fakeProvider{identity: identity}
	f := newHostedFixture(t, provider)
	ctx := t.Context()
	for _, prefix := range []string{"acme", "personal"} {
		if err := f.store.CreateWorkspace(ctx,
			&store.Workspace{Forge: "bitbucket", Prefix: prefix, Token: prefix + "-tok", DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
	}

	identity.OwnedWorkspaces = []string{"personal"}
	hostedSignIn(t, f, "/", "/")
	users, _ := f.store.ListUsers(ctx)
	want := map[string]store.Role{"acme": store.RoleMember, "personal": store.RoleOwner}
	if got := membershipRoles(t, f, users[0].ID); !reflect.DeepEqual(got, want) {
		t.Errorf("roles after first sign-in = %v, want %v", got, want)
	}

	// Swapped on the forge: promoted in acme, demoted in personal.
	identity.OwnedWorkspaces = []string{"acme"}
	hostedSignIn(t, f, "/", "/")
	want = map[string]store.Role{"acme": store.RoleOwner, "personal": store.RoleMember}
	if got := membershipRoles(t, f, users[0].ID); !reflect.DeepEqual(got, want) {
		t.Errorf("roles after re-sync = %v, want %v", got, want)
	}
}

// membershipRoles maps the user's memberships to their roles by workspace
// prefix.
func membershipRoles(t *testing.T, f *fixture, userID int64) map[string]store.Role {
	t.Helper()
	ctx := t.Context()
	ms, err := f.store.ListMembershipsForUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	wss, err := f.store.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prefixes := make(map[int64]string, len(wss))
	for _, ws := range wss {
		prefixes[ws.ID] = ws.Prefix
	}
	out := make(map[string]store.Role, len(ms))
	for _, m := range ms {
		out[prefixes[m.WorkspaceID]] = m.Role
	}
	return out
}

func TestHostedLoginPageHasNoDenialOrWorkspaceHints(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if err := f.store.CreateWorkspace(t.Context(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	// Even with the denial flag forced by URL, a hosted login page must
	// neither 403 nor disclose which workspaces the instance tracks.
	rec := get(f, "/login?denied=1")
	if rec.Code != http.StatusOK {
		t.Errorf("hosted /login?denied=1: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "no access to this instance") || strings.Contains(body, "<code>acme</code>") {
		t.Errorf("hosted login page shows denial content:\n%s", body)
	}
	if !strings.Contains(body, "Sign in to get started") {
		t.Errorf("hosted login page misses the getting-started copy:\n%s", body)
	}
}
