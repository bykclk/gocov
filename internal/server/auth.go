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

	"github.com/bykclk/gocov/internal/auth"
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

// authEnabled reports whether at least one sign-in provider is
// configured — the switch between the open UI and enforced sign-in.
func (s *Server) authEnabled() bool { return len(s.auths) > 0 }

// requireAuth is the enforcement middleware. With no provider configured
// the UI stays open exactly as before (the layout shows a banner instead);
// with one configured, every non-public path needs a valid session.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() || publicPath(r.URL.Path) {
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

// loginProvider is one sign-in button on the login page.
type loginProvider struct {
	Name  string // forge name, the URL segment of the login routes
	Label string // human-readable button label
}

// providerLabels maps forge names to their proper spelling; unknown
// forges fall back to their capitalized name.
var providerLabels = map[string]string{
	"bitbucket": "Bitbucket",
	"github":    "GitHub",
}

func providerLabel(name string) string {
	if l, ok := providerLabels[name]; ok {
		return l
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// handleLogin implements GET /login — the sign-in page with one button
// per provider, doubling as the generic-failure and access-denied page.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
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
	providers := make([]loginProvider, 0, len(s.authOrder))
	for _, p := range s.authOrder {
		providers = append(providers, loginProvider{Name: p.Name(), Label: providerLabel(p.Name())})
	}
	s.render(w, r, "login.html", map[string]any{
		"Failed":     r.FormValue("error") == "1",
		"Denied":     denied,
		"Next":       sanitizeNext(r.FormValue("next")),
		"Workspaces": workspaces,
		"Providers":  providers,
	})
}

// oauthProvider resolves the {forge} path segment of the login routes,
// writing the error response itself when there is no such provider.
func (s *Server) oauthProvider(w http.ResponseWriter, r *http.Request) auth.Provider {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}
	p, ok := s.auths[r.PathValue("forge")]
	if !ok {
		http.NotFound(w, r)
		return nil
	}
	return p
}

// handleOAuthStart implements GET /oauth/{forge}/start: it binds a fresh
// state to a short-lived cookie and forwards to the consent screen.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	provider := s.oauthProvider(w, r)
	if provider == nil {
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
	http.Redirect(w, r, provider.AuthorizeURL(state, s.redirectURI(provider.Name())), http.StatusFound)
}

// handleOAuthCallback implements GET /oauth/{forge}/callback per D4: it
// verifies the state, exchanges the code for an identity, applies the
// workspace-membership rule and only then provisions the user and session.
// The provider discards the forge tokens; nothing forge-side is stored.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := s.oauthProvider(w, r)
	if provider == nil {
		return
	}
	failed := func() { http.Redirect(w, r, "/login?error=1", http.StatusFound) }

	state, next := readStateCookie(r)
	clearCookie(w, stateCookie, s.secureCookies)
	code := r.FormValue("code")
	if r.FormValue("error") != "" || code == "" || state == "" || r.FormValue("state") != state {
		s.log.Warn("oauth callback rejected", "forge", provider.Name(),
			"forge_error", r.FormValue("error"), "state_ok", r.FormValue("state") == state && state != "")
		failed()
		return
	}

	id, err := provider.Identity(r.Context(), code, s.redirectURI(provider.Name()))
	if err != nil {
		s.log.Error("oauth identity", "forge", provider.Name(), "err", err)
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
		s.log.Warn("sign-in denied", "forge", provider.Name(), "account", id.DisplayName, "email", id.Email,
			"member_of", id.Workspaces, "allowed", sortedKeys(allowed))
		http.Redirect(w, r, "/login?denied=1", http.StatusFound)
		return
	}

	u := &store.User{
		Forge:       provider.Name(),
		ForgeUUID:   id.ForgeUUID,
		Email:       id.Email,
		DisplayName: id.DisplayName,
	}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		s.internalError(w, "provisioning user", err)
		return
	}
	if err := s.syncMemberships(r.Context(), u, id.Workspaces); err != nil {
		s.internalError(w, "syncing workspace memberships", err)
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
	s.log.Info("sign-in", "forge", provider.Name(), "user", u.DisplayName, "email", u.Email)
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
	if !s.authEnabled() {
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

// syncMemberships persists the user's workspace memberships (M2/D2): the
// tracked workspaces on this forge whose prefix the forge reports the user
// belongs to. It runs on every sign-in with full-sync semantics, so a
// membership the forge no longer reports is dropped at the next login.
// Matching is per provider (forge + prefix), so two forges sharing a prefix
// stay distinct tenants.
func (s *Server) syncMemberships(ctx context.Context, u *store.User, forgeWorkspaces []string) error {
	forgeSet := make(map[string]bool, len(forgeWorkspaces))
	for _, ws := range forgeWorkspaces {
		forgeSet[ws] = true
	}
	tracked, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	var ids []int64
	for _, ws := range tracked {
		if ws.Forge == u.Forge && forgeSet[ws.Prefix] {
			ids = append(ids, ws.ID)
		}
	}
	return s.store.SetUserWorkspaces(ctx, u.ID, ids)
}

// repoScope captures which repos a request may see (M2/R3). When scoped is
// false the instance runs in open mode (D5) and every repo is visible;
// otherwise a repo is visible only when its workspace prefix is a member
// prefix.
type repoScope struct {
	scoped   bool
	prefixes map[string]bool
}

// allows reports whether a namespaced repo slug falls within the scope.
func (rs repoScope) allows(slug string) bool {
	if !rs.scoped {
		return true
	}
	prefix, _, _ := strings.Cut(slug, "/")
	return rs.prefixes[prefix]
}

// userScope resolves the request user's workspace membership into a scope.
// Auth off → unscoped, so an open-mode instance behaves exactly as before
// M2. Auth on → the user's workspace prefixes; a missing user (which should
// not occur behind requireAuth) yields a deny-all scope rather than an open
// one.
func (s *Server) userScope(r *http.Request) (repoScope, error) {
	if !s.authEnabled() {
		return repoScope{scoped: false}, nil
	}
	prefixes := map[string]bool{}
	if u := currentUser(r); u != nil {
		wss, err := s.store.ListWorkspacesForUser(r.Context(), u.ID)
		if err != nil {
			return repoScope{}, err
		}
		for _, ws := range wss {
			prefixes[ws.Prefix] = true
		}
	}
	return repoScope{scoped: true, prefixes: prefixes}, nil
}

// canView reports whether the request may see the given repo slug. Callers
// that fail the check 404 (D3: a non-member must not learn a repo exists).
func (s *Server) canView(r *http.Request, slug string) (bool, error) {
	scope, err := s.userScope(r)
	if err != nil {
		return false, err
	}
	return scope.allows(slug), nil
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

func (s *Server) redirectURI(forge string) string {
	return strings.TrimSuffix(s.baseURL, "/") + "/oauth/" + forge + "/callback"
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
