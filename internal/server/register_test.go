package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/auth"
	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/forge"
	forgefake "github.com/bykclk/gocov/internal/forge/fake"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
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
		Forges:  map[string]forge.Factory{"bitbucket": forgefake.New().Factory()},
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

	sess := hostedSignIn(t, f, "/", "/register")

	// The user exists with the login-time workspace snapshot (D3).
	users, err := f.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	if !reflect.DeepEqual(users[0].ForgeWorkspaces, []string{"acme", "personal"}) {
		t.Errorf("stored forge workspaces = %v", users[0].ForgeWorkspaces)
	}

	// The dashboard bounces a membership-less hosted user to registration.
	if rec := get(f, "/", sess); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/register" {
		t.Errorf("index: %d -> %q, want redirect to /register", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHostedUserWithMembershipLandsOnIndex(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if err := f.store.CreateWorkspace(context.Background(),
		&store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	sess := hostedSignIn(t, f, "/", "/")
	if rec := get(f, "/", sess); rec.Code != http.StatusOK {
		t.Errorf("index for member: status = %d", rec.Code)
	}
}

func TestRegistrationIsHostedOnly(t *testing.T) {
	// Private mode (M3/D1): no registration UI, byte-identical denial flow.
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)
	sess := signIn(t, f, "/")

	if rec := get(f, "/register", sess); rec.Code != http.StatusNotFound {
		t.Errorf("GET /register in private mode: status = %d, want 404", rec.Code)
	}
	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusNotFound {
		t.Errorf("POST /register in private mode: status = %d, want 404", rec.Code)
	}
}

func TestRegisterPageStates(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	ctx := context.Background()
	// "personal" is free; "acme" is taken by a GitHub workspace of the
	// same name, so it must render unavailable.
	if err := f.store.CreateWorkspace(ctx,
		&store.Workspace{Forge: "github", Prefix: "acme", Token: "gh-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	sess := hostedSignIn(t, f, "/", "/register")
	rec := get(f, "/register", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("register page: status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "personal") || !strings.Contains(body, "not registered yet") {
		t.Errorf("free workspace not offered:\n%s", body)
	}
	if !strings.Contains(body, "name in use under another forge") {
		t.Errorf("cross-forge collision not surfaced:\n%s", body)
	}
}

func TestRegisterCreatesWorkspaceAndShowsTokenOnce(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	sess := hostedSignIn(t, f, "/", "/register")

	rec := postRegister(f, "personal", sess)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces/personal/setup" {
		t.Fatalf("register: %d -> %q, want redirect to the setup page", rec.Code, rec.Header().Get("Location"))
	}

	ctx := context.Background()
	ws, err := f.store.WorkspaceByPrefix(ctx, "personal")
	if err != nil {
		t.Fatalf("workspace not created: %v", err)
	}
	if ws.Forge != "bitbucket" || ws.Token == "" || ws.DefaultBranch != "main" {
		t.Errorf("workspace = %+v", ws)
	}
	// The onboarding page shows the token in the CI snippet (D6).
	setup := get(f, "/workspaces/personal/setup", sess)
	if setup.Code != http.StatusOK || !strings.Contains(setup.Body.String(), ws.Token) {
		t.Errorf("setup page (status %d) does not show the upload token:\n%s", setup.Code, setup.Body)
	}

	// Registration made the user a member atomically (R2)...
	users, _ := f.store.ListUsers(ctx)
	wss, err := f.store.ListWorkspacesForUser(ctx, users[0].ID)
	if err != nil || len(wss) != 1 || wss[0].Prefix != "personal" {
		t.Fatalf("memberships after registration = %v, %v", wss, err)
	}
	// ...so the dashboard opens instead of bouncing back to /register.
	if rec := get(f, "/", sess); rec.Code != http.StatusOK {
		t.Errorf("index after registration: status = %d", rec.Code)
	}

	// The register page now shows the workspace as joined, without the token.
	page := get(f, "/register", sess)
	if !strings.Contains(page.Body.String(), "member") {
		t.Errorf("register page misses the member state:\n%s", page.Body)
	}
	if strings.Contains(page.Body.String(), ws.Token) {
		t.Error("register page leaks the upload token")
	}
}

func TestRegisterRejectsForeignPrefix(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	sess := hostedSignIn(t, f, "/", "/register")

	// "evilcorp" is not in the identity's forge workspace list; the form
	// value must be rejected server-side regardless of what was posted.
	for _, prefix := range []string{"evilcorp", ""} {
		if rec := postRegister(f, prefix, sess); rec.Code != http.StatusForbidden {
			t.Errorf("register %q: status = %d, want 403", prefix, rec.Code)
		}
	}
	if _, err := f.store.WorkspaceByPrefix(context.Background(), "evilcorp"); err == nil {
		t.Error("rejected registration created a workspace")
	}
}

func TestRegisterCrossForgeCollisionConflicts(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if err := f.store.CreateWorkspace(context.Background(),
		&store.Workspace{Forge: "github", Prefix: "acme", Token: "gh-tok", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	sess := hostedSignIn(t, f, "/", "/register")

	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusConflict {
		t.Errorf("cross-forge claim: status = %d, want 409", rec.Code)
	}
}

func TestRegisterAlreadyRegisteredJoins(t *testing.T) {
	// A colleague registered "acme" after this user's login sync; claiming
	// it is a non-event (D2): membership is granted, no new workspace.
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	sess := hostedSignIn(t, f, "/", "/register")
	ctx := context.Background()
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
	// Idempotent: joining again keeps the single membership.
	if rec := postRegister(f, "acme", sess); rec.Code != http.StatusFound {
		t.Errorf("second join: status = %d", rec.Code)
	}
	if wss, _ := f.store.ListWorkspacesForUser(ctx, users[0].ID); len(wss) != 1 {
		t.Errorf("second join changed memberships: %v", wss)
	}
}

func TestReLoginRefreshesForgeWorkspaces(t *testing.T) {
	provider := &fakeProvider{identity: memberIdentity()}
	f := newHostedFixture(t, provider)
	hostedSignIn(t, f, "/", "/register")

	provider.identity.Workspaces = []string{"acme", "personal", "newco"}
	hostedSignIn(t, f, "/", "/register")

	users, _ := f.store.ListUsers(context.Background())
	if len(users) != 1 || !reflect.DeepEqual(users[0].ForgeWorkspaces, []string{"acme", "personal", "newco"}) {
		t.Errorf("stored list not refreshed: %+v", users)
	}
}

func TestHostedLoginPageHasNoDenialOrWorkspaceHints(t *testing.T) {
	f := newHostedFixture(t, &fakeProvider{identity: memberIdentity()})
	if err := f.store.CreateWorkspace(context.Background(),
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
