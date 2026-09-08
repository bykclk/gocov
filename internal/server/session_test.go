package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gocov/gocov/internal/store"
)

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

	rec := get(f, "/repos/bitbucket/acme/widgets?branch=main")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/login" {
		t.Fatalf("location = %q, want /login", rec.Header().Get("Location"))
	}
	if next := loc.Query().Get("next"); next != "/repos/bitbucket/acme/widgets?branch=main" {
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
		"/healthz":                          http.StatusOK,
		"/badge/bitbucket/acme/widgets.svg": http.StatusOK,
		"/static/style.css":                 http.StatusOK,
		"/login":                            http.StatusOK,
	} {
		if rec := get(f, path); rec.Code != want {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, want)
		}
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
	if err := f.store.UpsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateSession(t.Context(), &store.Session{
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
