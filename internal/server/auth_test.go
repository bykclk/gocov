package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bykclk/gocov/internal/auth"
	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/forge"
	forgefake "github.com/bykclk/gocov/internal/forge/fake"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

// fakeProvider is an auth.Provider whose Identity is canned.
type fakeProvider struct {
	identity *auth.Identity
	err      error
	gotCode  string
}

func (f *fakeProvider) Name() string { return "bitbucket" }
func (f *fakeProvider) AuthorizeURL(state, redirectURI string) string {
	return "https://bb.example/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}
func (f *fakeProvider) Identity(_ context.Context, code, _ string) (*auth.Identity, error) {
	f.gotCode = code
	return f.identity, f.err
}

func memberIdentity() *auth.Identity {
	return &auth.Identity{
		ForgeUUID:   "{uuid-1}",
		DisplayName: "Jane Dev",
		Email:       "jane@example.com",
		Workspaces:  []string{"acme", "personal"},
	}
}

// newAuthFixture builds a server with sign-in enabled (unless provider is
// nil) over a store tracking the repo acme/widgets.
func newAuthFixture(t *testing.T, provider auth.Provider, allowed []string) *fixture {
	t.Helper()
	st := storemem.New()
	repo := &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token", DefaultBranch: "main"}
	if err := st.CreateRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Store:             st,
		Blobs:             blobmem.New(),
		Parsers:           map[string]profile.Parser{"go": profile.GoParser{}},
		Forges:            map[string]forge.Factory{"bitbucket": forgefake.New().Factory()},
		BaseURL:           "https://gocov.example",
		Auth:              provider,
		AllowedWorkspaces: allowed,
	})
	return &fixture{srv: srv, store: st, repo: repo}
}

func get(f *fixture, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", name)
	return nil
}

// signIn drives the full flow and returns the session cookie.
func signIn(t *testing.T, f *fixture, next string) *http.Cookie {
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
	if loc := cb.Header().Get("Location"); loc != sanitizeNext(next) {
		t.Fatalf("callback redirected to %q, want %q", loc, sanitizeNext(next))
	}
	return cookieNamed(t, cb, sessionCookie)
}

func TestAuthDisabledKeepsUIOpen(t *testing.T) {
	f := newAuthFixture(t, nil, nil)

	rec := get(f, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index: status = %d, want 200 without auth configured", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GOCOV_OAUTH_BITBUCKET_KEY") {
		t.Error("open UI must show the enable-sign-in banner")
	}
	if rec := get(f, "/login"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Errorf("login with auth disabled: %d -> %q, want redirect to /", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthEnforcedRedirectsToLogin(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)

	rec := get(f, "/repos/acme/widgets?branch=main")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/login" {
		t.Fatalf("location = %q, want /login", rec.Header().Get("Location"))
	}
	if next := loc.Query().Get("next"); next != "/repos/acme/widgets?branch=main" {
		t.Errorf("next = %q, want original path+query", next)
	}
	// The banner belongs to the open state only.
	if login := get(f, "/login"); strings.Contains(login.Body.String(), "GOCOV_OAUTH_BITBUCKET_KEY") {
		t.Error("banner shown although sign-in is configured")
	}
}

func TestAuthEnforcedPublicEndpointsStayPublic(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)

	// Upload API: byte-identical behavior, still token-authed only.
	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Errorf("upload with auth enabled: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec := doUpload(t, f, "wrong", map[string]string{"commit": "c1"}, testProfile); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token upload: status = %d, want 401 (not a login redirect)", rec.Code)
	}

	for path, want := range map[string]int{
		"/healthz":                http.StatusOK,
		"/badge/acme/widgets.svg": http.StatusOK,
		"/static/style.css":       http.StatusOK,
		"/login":                  http.StatusOK,
	} {
		if rec := get(f, path); rec.Code != want {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	provider := &fakeProvider{identity: memberIdentity()}
	f := newAuthFixture(t, provider, nil)

	sess := signIn(t, f, "/repos/acme/widgets")
	if provider.gotCode != "thecode" {
		t.Errorf("provider got code %q", provider.gotCode)
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || !sess.Secure {
		t.Errorf("session cookie flags: %+v", sess)
	}

	// The session works and the page shows the signed-in user.
	rec := get(f, "/", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated index: status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Jane Dev") || !strings.Contains(rec.Body.String(), "Sign out") {
		t.Error("page must show the user chip and sign-out button")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on protected pages", cc)
	}

	// JIT provisioning: exactly one user row, refreshed on re-login.
	users, err := f.store.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("users = %v, %v", users, err)
	}
	if users[0].ForgeUUID != "{uuid-1}" || users[0].Email != "jane@example.com" {
		t.Errorf("user = %+v", users[0])
	}
	provider.identity.DisplayName = "Jane Renamed"
	signIn(t, f, "/")
	users, _ = f.store.ListUsers(context.Background())
	if len(users) != 1 || users[0].DisplayName != "Jane Renamed" {
		t.Errorf("second login: users = %+v, want 1 row with refreshed name", users)
	}

	// /login while signed in goes straight back in.
	if rec := get(f, "/login", sess); rec.Code != http.StatusFound {
		t.Errorf("login while signed in: status = %d, want redirect", rec.Code)
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)

	start := get(f, "/oauth/bitbucket/start")
	stateCk := cookieNamed(t, start, stateCookie)

	for name, path := range map[string]string{
		"missing state":    "/oauth/bitbucket/callback?code=x",
		"mismatched state": "/oauth/bitbucket/callback?code=x&state=wrong",
		"missing code":     "/oauth/bitbucket/callback?state=" + url.QueryEscape(strings.SplitN(stateCk.Value, "|", 2)[0]),
		"forge error":      "/oauth/bitbucket/callback?error=access_denied",
	} {
		rec := get(f, path, stateCk)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
			t.Errorf("%s: %d -> %q, want redirect to /login?error=1", name, rec.Code, rec.Header().Get("Location"))
		}
	}
	// A callback with no state cookie at all fails too.
	rec := get(f, "/oauth/bitbucket/callback?code=x&state=anything")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("no cookie: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if users, _ := f.store.ListUsers(context.Background()); len(users) != 0 {
		t.Errorf("rejected callbacks must not create users, got %v", users)
	}
}

func TestCallbackProviderFailure(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{err: errors.New("bitbucket down")}, nil)
	start := get(f, "/oauth/bitbucket/start")
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")

	rec := get(f, "/oauth/bitbucket/callback?code=x&state="+url.QueryEscape(state), stateCk)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
		t.Errorf("%d -> %q, want generic failure redirect", rec.Code, rec.Header().Get("Location"))
	}
}

func TestNonMemberIsDenied(t *testing.T) {
	outsider := &auth.Identity{ForgeUUID: "{uuid-2}", DisplayName: "Mallory", Workspaces: []string{"other"}}
	f := newAuthFixture(t, &fakeProvider{identity: outsider}, nil)

	start := get(f, "/oauth/bitbucket/start")
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")
	rec := get(f, "/oauth/bitbucket/callback?code=x&state="+url.QueryEscape(state), stateCk)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?denied=1" {
		t.Fatalf("%d -> %q, want denial redirect", rec.Code, rec.Header().Get("Location"))
	}

	// R3: no user row, no session.
	if users, _ := f.store.ListUsers(context.Background()); len(users) != 0 {
		t.Errorf("denied login created users: %v", users)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Error("denied login set a session cookie")
		}
	}

	// The denial page explains the workspace-membership rule, with 403.
	denied := get(f, "/login?denied=1")
	if denied.Code != http.StatusForbidden {
		t.Errorf("denial page status = %d, want 403", denied.Code)
	}
	if !strings.Contains(denied.Body.String(), "no access to this instance") {
		t.Error("denial page must explain the membership rule")
	}
}

func TestAllowedWorkspacesOverride(t *testing.T) {
	// The identity is a member of "acme" (tracked) but the operator
	// restricted sign-in to "vip" only.
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, []string{"vip"})

	start := get(f, "/oauth/bitbucket/start")
	stateCk := cookieNamed(t, start, stateCookie)
	state, _, _ := strings.Cut(stateCk.Value, "|")
	rec := get(f, "/oauth/bitbucket/callback?code=x&state="+url.QueryEscape(state), stateCk)
	if rec.Header().Get("Location") != "/login?denied=1" {
		t.Errorf("override ignored: redirected to %q", rec.Header().Get("Location"))
	}
}

func TestOpenRedirectRejected(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)
	for _, next := range []string{"https://evil.example", "//evil.example", `/\evil.example`, "no-slash"} {
		sess := signIn(t, f, next) // signIn asserts the redirect goes to sanitizeNext(next)
		if got := sanitizeNext(next); got != "/" {
			t.Errorf("sanitizeNext(%q) = %q, want /", next, got)
		}
		_ = sess
	}
}

func TestLogoutKillsSessionServerSide(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)
	sess := signIn(t, f, "/")

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("logout: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	// A saved copy of the cookie must not restore access (R3).
	if rec := get(f, "/", sess); rec.Code != http.StatusFound {
		t.Errorf("old session cookie still works after logout: status = %d", rec.Code)
	}
}

func TestExpiredSessionDoesNotAuthenticate(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)
	u := &store.User{Forge: "bitbucket", ForgeUUID: "{u}", Email: "e@x", DisplayName: "E"}
	if err := f.store.UpsertUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateSession(context.Background(), &store.Session{
		TokenHash: hashToken("expired-token"),
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	rec := get(f, "/", &http.Cookie{Name: sessionCookie, Value: "expired-token"})
	if rec.Code != http.StatusFound {
		t.Errorf("expired session: status = %d, want login redirect", rec.Code)
	}
}

func TestLoginPageHidesWorkspacesUntilDenied(t *testing.T) {
	f := newAuthFixture(t, &fakeProvider{identity: memberIdentity()}, nil)

	// The plain sign-in page is reachable without any session; the
	// tracked-workspace slugs must not leak to strangers there.
	rec := get(f, "/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<code>acme</code>") {
		t.Errorf("unauthenticated login page lists tracked workspaces:\n%s", rec.Body)
	}

	// After a real Bitbucket identity was rejected, the list helps the
	// denied member ask for access to the right workspace.
	rec = get(f, "/login?denied=1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<code>acme</code>") {
		t.Errorf("denied page misses the tracked workspaces:\n%s", rec.Body)
	}
}
