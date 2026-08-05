package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bykclk/gocov/internal/store"
)

// Session lifetime is fixed (no sliding renewal in M1); logout and
// `gocov-server user remove` revoke server-side immediately.
const sessionTTL = 30 * 24 * time.Hour

const (
	sessionCookie = "gocov_session"
	// stateCookie binds the OAuth callback to the browser that started the
	// flow (CSRF/replay protection). It also carries the in-site path to
	// return to after login.
	stateCookie = "gocov_oauth_state"
)

type ctxKey int

const userKey ctxKey = 0

// currentUser returns the signed-in user, or nil on public pages and when
// auth is not configured.
func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func withUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// publicPath reports whether a path must work without a session: the CI
// surface (upload API, badges, health), embedded assets and the login
// flow itself. Everything else is a protected page.
func publicPath(p string) bool {
	if p == "/api/v1/upload" || p == "/healthz" || p == "/login" {
		return true
	}
	return strings.HasPrefix(p, "/badge/") ||
		strings.HasPrefix(p, "/static/") ||
		strings.HasPrefix(p, "/oauth/")
}

// requireAuth is the enforcement middleware. With no provider configured
// the UI stays open exactly as before (the layout shows a banner instead);
// with one configured, every non-public path needs a valid session.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil || publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u := s.sessionUser(r)
		if u == nil {
			redirectToLogin(w, r)
			return
		}
		// Protected pages must not be served from cache after logout.
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// sessionUser resolves the session cookie to a user, or nil.
func (s *Server) sessionUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.store.UserBySession(r.Context(), hashToken(c.Value))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("session lookup", "err", err)
		}
		return nil
	}
	return u
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
}

// handleLogin implements GET /login — the "Sign in with Bitbucket" page,
// doubling as the generic-failure and access-denied page.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if u := s.sessionUser(r); u != nil {
		http.Redirect(w, r, sanitizeNext(r.FormValue("next")), http.StatusFound)
		return
	}
	denied := r.FormValue("denied") == "1"
	if denied {
		w.WriteHeader(http.StatusForbidden)
	}
	// The tracked-workspace slugs tell a denied member which workspace
	// to ask about, but to anyone else they disclose who uses this
	// instance — so they render only after a real Bitbucket identity
	// was rejected, never on the plain sign-in page.
	var workspaces []string
	if denied {
		workspaces = s.trackedWorkspaces(r)
	}
	s.render(w, r, "login.html", map[string]any{
		"Failed":     r.FormValue("error") == "1",
		"Denied":     denied,
		"Next":       sanitizeNext(r.FormValue("next")),
		"Workspaces": workspaces,
	})
}

// handleOAuthStart implements GET /oauth/bitbucket/start: it binds a fresh
// state to a short-lived cookie and forwards to the consent screen.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state, err := newState()
	if err != nil {
		s.internalError(w, "generating oauth state", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state + "|" + sanitizeNext(r.FormValue("next")),
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.auth.AuthorizeURL(state, s.redirectURI()), http.StatusFound)
}

// handleOAuthCallback implements GET /oauth/bitbucket/callback per D4: it
// verifies the state, exchanges the code for an identity, applies the
// workspace-membership rule and only then provisions the user and session.
// The provider discards the Bitbucket tokens; nothing forge-side is stored.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	failed := func() { http.Redirect(w, r, "/login?error=1", http.StatusFound) }

	state, next := readStateCookie(r)
	clearCookie(w, stateCookie, s.secureCookies)
	code := r.FormValue("code")
	if r.FormValue("error") != "" || code == "" || state == "" || r.FormValue("state") != state {
		s.log.Warn("oauth callback rejected", "forge", s.auth.Name(),
			"forge_error", r.FormValue("error"), "state_ok", r.FormValue("state") == state && state != "")
		failed()
		return
	}

	id, err := s.auth.Identity(r.Context(), code, s.redirectURI())
	if err != nil {
		s.log.Error("oauth identity", "forge", s.auth.Name(), "err", err)
		failed()
		return
	}

	allowed, err := s.allowedWorkspaceSet(r)
	if err != nil {
		s.internalError(w, "deriving allowed workspaces", err)
		return
	}
	member := false
	for _, ws := range id.Workspaces {
		if allowed[ws] {
			member = true
			break
		}
	}
	if !member {
		// No user row, no session (R3): denial must leave nothing behind.
		// Both sides of the failed intersection are logged so an operator
		// can see at a glance whether the fix is a missing registration,
		// a stale GOCOV_ALLOWED_WORKSPACES or a slug mismatch.
		s.log.Warn("sign-in denied", "forge", s.auth.Name(), "account", id.DisplayName, "email", id.Email,
			"member_of", id.Workspaces, "allowed", sortedKeys(allowed))
		http.Redirect(w, r, "/login?denied=1", http.StatusFound)
		return
	}

	u := &store.User{
		Forge:       s.auth.Name(),
		ForgeUUID:   id.ForgeUUID,
		Email:       id.Email,
		DisplayName: id.DisplayName,
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.internalError(w, "provisioning user", err)
		return
	}
	token, err := newState() // same entropy requirement: 256 random bits
	if err != nil {
		s.internalError(w, "generating session token", err)
		return
	}
	sess := &store.Session{
		TokenHash: hashToken(token),
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(sessionTTL),
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		s.internalError(w, "creating session", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	s.log.Info("sign-in", "forge", s.auth.Name(), "user", u.DisplayName, "email", u.Email)
	http.Redirect(w, r, next, http.StatusFound)
}

// handleLogout implements POST /logout: the session dies server-side, so a
// saved cookie or the back button cannot restore access.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, "deleting session", err)
			return
		}
	}
	clearCookie(w, sessionCookie, s.secureCookies)
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// allowedWorkspaceSet is the D3 authorization rule: the operator's explicit
// GOCOV_ALLOWED_WORKSPACES list when set, otherwise the workspaces this
// instance tracks (registered workspace prefixes plus the workspace part
// of every registered repo slug).
func (s *Server) allowedWorkspaceSet(r *http.Request) (map[string]bool, error) {
	set := map[string]bool{}
	if len(s.allowedWorkspaces) > 0 {
		for _, ws := range s.allowedWorkspaces {
			set[ws] = true
		}
		return set, nil
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nil, err
	}
	for _, ws := range workspaces {
		set[ws.Prefix] = true
	}
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if prefix, _, ok := strings.Cut(repo.Slug, "/"); ok {
			set[prefix] = true
		}
	}
	return set, nil
}

// trackedWorkspaces renders the allowed set for the login page, so it is
// obvious whose coverage an instance holds.
func (s *Server) trackedWorkspaces(r *http.Request) []string {
	set, err := s.allowedWorkspaceSet(r)
	if err != nil {
		return nil
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) redirectURI() string {
	return strings.TrimSuffix(s.baseURL, "/") + "/oauth/bitbucket/callback"
}

// sanitizeNext confines the post-login redirect to in-site paths, so the
// login URL can never be turned into an open redirect.
func sanitizeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func readStateCookie(r *http.Request) (state, next string) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		return "", "/"
	}
	state, next, _ = strings.Cut(c.Value, "|")
	return state, sanitizeNext(next)
}

func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// newState returns 256 random bits hex-encoded, used for both the OAuth
// state and session tokens.
func newState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashToken is what the sessions table stores instead of the token: a DB
// leak then reveals nothing that authenticates.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
