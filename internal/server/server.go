// Package server implements the gocov HTTP API, badge endpoint and web UI.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gocov/gocov/internal/auth"
	"github.com/gocov/gocov/internal/blobstore"
	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/diffcov"
	"github.com/gocov/gocov/internal/oidc"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Config wires the server's dependencies. All fields are required except
// Logger, BaseURL and Health.
type Config struct {
	Store   store.Store
	Blobs   blobstore.Store
	Parsers map[string]profile.Parser // by format name, e.g. "go"
	BaseURL string                    // public URL of this server, for links in build statuses
	Logger  *slog.Logger
	// Health is probed by GET /healthz (e.g. a database ping).
	// When nil, /healthz always reports healthy.
	Health func(ctx context.Context) error
	// Auths enables web UI sign-in, one provider per forge, rendered as
	// one login button each. Empty keeps the UI open (with a banner
	// explaining how to enable sign-in); the upload API, badges and health
	// checks are unaffected either way.
	Auths []auth.Provider
	// AllowedWorkspaces overrides the derived "tracked workspaces" set
	// that gates who may sign in. Empty means derive from the store.
	AllowedWorkspaces []string
	// Hosted switches the instance to self-service mode (M3/D1): any
	// forge account may sign in, and users without a tracked-workspace
	// membership are routed to the registration page instead of being
	// denied. False keeps today's private behavior exactly.
	Hosted bool
	// GitHubApp is the deployment's GitHub App identity (One-Click
	// Connect P1), implemented by forge/github.App. Nil when the
	// deployment has none; the credential chain then starts at repo
	// credentials exactly as before.
	GitHubApp GitHubApp
	// BitbucketConnect is the OAuth consumer powering the Bitbucket
	// workspace-connect grant (One-Click Connect P2), implemented by
	// forge/bitbucket.Consumer. Nil disables the feature; requires
	// GOCOV_SECRET_KEY at the store for the at-rest token encryption.
	BitbucketConnect BitbucketConnect
	// GitLabConnect is the OAuth application powering the GitLab
	// workspace-connect grant, implemented by forge/gitlab.Application.
	// Nil disables the feature; requires GOCOV_SECRET_KEY at the store
	// for the at-rest token encryption, and the application must carry
	// the "api" scope on top of sign-in's read scopes.
	GitLabConnect GitLabConnect
	// GitHubWebhookSecret enables the GitHub App / Marketplace webhook
	// (POST /github/webhook) and is the HMAC secret its signatures are
	// verified against. Empty leaves the route unregistered.
	GitHubWebhookSecret string
	// PublicReports allows anonymous read-only report pages for repos the
	// forge reports public (GOCOV_PUBLIC_REPORTS). False keeps every page
	// behind the login wall exactly as before.
	PublicReports bool
	// OIDCVerifier verifies the forge-minted OIDC identity tokens that let a
	// repo's own CI upload without a pasted token (server/oidc.go). Nil
	// builds the default: the public forge issuers, with this server's
	// BaseURL as the required audience. Tests inject one pointed at a local
	// issuer.
	OIDCVerifier *oidc.Verifier
	// OIDCIssuers lists extra trusted OIDC issuers beyond the public forge
	// ones (GOCOV_OIDC_ISSUERS): self-managed GitLab instance URLs whose CI
	// ID tokens name repos by project_path, the same as gitlab.com.
	OIDCIssuers []string
	// PostHog turns on the browser analytics snippet in the web UI
	// (analytics.go). The zero value leaves every page free of third-party
	// scripts, which is what self-hosted deployments get by default.
	PostHog PostHog
}

// The forge connectors a deployment can configure. They are declared in
// internal/core, which owns the connections and their upkeep; the aliases
// keep this package's Config the one place a caller has to look.
type (
	GitHubApp        = core.GitHubApp
	BitbucketConnect = core.BitbucketConnect
	GitLabConnect    = core.GitLabConnect
)

// Server is the gocov HTTP server.
type Server struct {
	store         store.Store
	blobs         blobstore.Store
	parsers       map[string]profile.Parser
	baseURL       string
	log           *slog.Logger
	pages         map[string]*template.Template
	mux           *http.ServeMux
	handler       http.Handler // mux wrapped in the auth middleware
	health        func(ctx context.Context) error
	forges        *core.Forges
	webhookSecret string
	// pipeline is the coverage logic proper: gate, merge, forge report.
	pipeline *core.Pipeline
	// tokenless rate-limits tokenless upload attempts per repo.
	tokenless *tokenlessLimiter
	// oidc verifies forge-minted OIDC identity tokens for tokenless uploads
	// from a repo's own CI (server/oidc.go).
	oidc *oidc.Verifier
	// gitlabIssuers is the set of trusted GitLab OIDC issuers — gitlab.com
	// plus any operator-configured self-managed instances — used to route a
	// verified token to the gitlab claim mapping.
	gitlabIssuers map[string]bool

	// auths holds the sign-in providers by forge name; authOrder keeps
	// the configured order for the login-page buttons.
	auths             map[string]auth.Provider
	authOrder         []auth.Provider
	allowedWorkspaces []string
	hosted            bool
	publicReports     bool
	// secureCookies marks auth cookies Secure when the public base URL is
	// https (the UI is then served through TLS or a terminating proxy).
	secureCookies bool
	// posthog is the analytics snippet configuration; zero means off.
	posthog PostHog
}

// New builds a Server; panics only on programmer error (bad templates).
func New(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	assetVer := staticVersion()
	funcs := template.FuncMap{
		"pct":      func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
		"short":    shortSHA,
		"sub":      func(a, b int64) int64 { return a - b },
		"humanint": humanInt,
		"ranges":   diffcov.Ranges,
		"covclass": covClass,
		"timeago":  timeAgo,
		// asset appends a content-derived version so browsers refetch
		// embedded assets after a server upgrade despite long cache TTLs.
		"asset": func(path string) string { return path + "?v=" + assetVer },
		// pathesc encodes a value into a single URL path segment; GitLab
		// workspace prefixes contain slashes and must ride as %2F.
		"pathesc": url.PathEscape,
		// The tenant URL builders (urls.go): a repo's page, badge and
		// settings, and a workspace's pages, forge segment included.
		"repourl":     repoURL,
		"badgeurl":    badgeURL,
		"settingsurl": repoSettingsURL,
		"wsurl":       workspaceURL,
		// forgeicon renders a forge's brand mark for the sign-in button.
		"forgeicon": providerIcon,
		// dict builds a map from alternating key/value args so a shared
		// sub-template (e.g. the coverage-gate row) can be invoked with
		// named fields inline.
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not a string", i)
				}
				m[k] = pairs[i+1]
			}
			return m, nil
		},
	}
	// Every page is its own template set sharing the layout and partials,
	// so pages can define "content" without colliding.
	pages := map[string]*template.Template{}
	for _, name := range []string{"index.html", "repo.html", "repo-settings.html", "upload.html", "source.html",
		"login.html", "workspace.html", "onboarding.html", "connect.html", "not-found.html"} {
		pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(templatesFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name))
	}

	// The trusted GitLab issuers. A gocov deployment connects to exactly one
	// GitLab, so it must trust exactly that instance's issuer — not gitlab.com
	// *and* a self-managed one at once, which would let a token from either
	// authenticate an upload to a same-named project on the other (GitLab
	// resolves by project path, and paths are not unique across instances).
	// So the operator's configured issuers replace the gitlab.com default
	// rather than adding to it: unset means gitlab.com; set means exactly the
	// listed self-managed instances. GitHub and Bitbucket are unaffected.
	// Both the token router (oidcForge) and the verifier's issuer allowlist
	// draw from this set.
	gitlabIssuers := map[string]bool{}
	for _, iss := range cfg.OIDCIssuers {
		if iss = strings.TrimRight(iss, "/"); iss != "" {
			gitlabIssuers[iss] = true
		}
	}
	if len(gitlabIssuers) == 0 {
		gitlabIssuers[gitLabDotComIssuer] = true
	}

	oidcVerifier := cfg.OIDCVerifier
	if oidcVerifier == nil && cfg.BaseURL != "" {
		// A token's aud must equal this server's public URL, so a token
		// minted for another instance cannot be replayed here. Without a
		// BaseURL there is no audience to bind to, so OIDC uploads stay off.
		exactIssuers := []string{gitHubActionsIssuer}
		for iss := range gitlabIssuers {
			exactIssuers = append(exactIssuers, iss)
		}
		oidcVerifier = oidc.New(oidc.Config{
			Audience:      cfg.BaseURL,
			Issuers:       exactIssuers,
			ResolveIssuer: bitbucketIssuerResolver(cfg.Store),
		})
	}

	s := &Server{
		store:         cfg.Store,
		blobs:         cfg.Blobs,
		parsers:       cfg.Parsers,
		baseURL:       cfg.BaseURL,
		log:           log,
		pages:         pages,
		mux:           http.NewServeMux(),
		health:        cfg.Health,
		forges:        core.NewForges(cfg.Store, log, cfg.BaseURL, cfg.GitHubApp, cfg.BitbucketConnect, cfg.GitLabConnect),
		webhookSecret: cfg.GitHubWebhookSecret,
		tokenless:     newTokenlessLimiter(),
		oidc:          oidcVerifier,
		gitlabIssuers: gitlabIssuers,

		auths:             map[string]auth.Provider{},
		authOrder:         cfg.Auths,
		allowedWorkspaces: cfg.AllowedWorkspaces,
		hosted:            cfg.Hosted,
		publicReports:     cfg.PublicReports,
		secureCookies:     strings.HasPrefix(cfg.BaseURL, "https://"),
		posthog:           cfg.PostHog,
	}
	// Everything that decides rather than transports lives in core; the
	// server holds one handle to it.
	s.pipeline = &core.Pipeline{Store: cfg.Store, Blobs: cfg.Blobs, Log: log, BaseURL: cfg.BaseURL, Forges: s.forges, Hosted: cfg.Hosted}
	for _, p := range cfg.Auths {
		s.auths[p.Name()] = p
	}
	s.routes()
	s.handler = s.requireAuth(s.mux)
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v1/upload", s.handleUpload)
	s.mux.HandleFunc("GET /badge/{forge}/{slug...}", s.handleBadge)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /robots.txt", s.handleRobots)
	s.mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	s.mux.Handle("GET /static/", cacheStatic(http.FileServerFS(staticFS)))
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /oauth/{forge}/start", s.handleOAuthStart)
	s.mux.HandleFunc("GET /oauth/{forge}/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.HandleFunc("GET /onboarding", s.handleOnboarding)
	s.mux.HandleFunc("GET /register", s.handleRegisterPage)
	s.mux.HandleFunc("POST /register", s.handleRegister)
	// Tenant pages carry the forge before the name: repo slugs and
	// workspace prefixes are unique per forge, not globally (urls.go).
	s.mux.HandleFunc("GET /workspaces/{forge}/{prefix}", s.handleWorkspacePage)
	s.mux.HandleFunc("POST /workspaces/{forge}/{prefix}/rotate-token", s.handleWorkspaceRotate)
	s.mux.HandleFunc("POST /workspaces/{forge}/{prefix}/settings", s.handleWorkspaceSettings)
	s.mux.HandleFunc("POST /workspaces/{forge}/{prefix}/delete", s.handleWorkspaceDelete)
	// The workspace's forge decides what connect and disconnect mean: the
	// GitHub App link, or the Bitbucket/GitLab consent grant.
	s.mux.HandleFunc("GET /workspaces/{forge}/{prefix}/connect", s.handleConnect)
	s.mux.HandleFunc("POST /workspaces/{forge}/{prefix}/disconnect", s.handleDisconnect)
	s.mux.HandleFunc("GET /workspaces/{forge}/{prefix}/setup", s.handleWorkspaceSetup)
	s.mux.HandleFunc("GET /workspaces/{forge}/{prefix}/setup/status", s.handleWorkspaceSetupStatus)
	s.mux.HandleFunc("GET /github/setup", s.handleGitHubSetup)
	if s.webhookSecret != "" {
		s.mux.HandleFunc("POST /github/webhook", s.handleGitHubWebhook)
	}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	// Least-specific pattern: anything no route above claims lands here. A
	// browser GET gets the styled 404 page; other methods/clients keep the
	// plain-text 404 (so an unmatched path 404s rather than 405s).
	s.mux.HandleFunc("/", s.handleNotFound)
	// Repo slugs contain a slash (workspace/repo), so the slug must ride as a
	// trailing {slug...} wildcard — a single {slug} segment cannot match it on
	// a live server (only httptest preserves the %2F). The mutating actions
	// therefore carry their verb before the slug rather than after it.
	s.mux.HandleFunc("GET /repo-settings/{forge}/{slug...}", s.handleRepoSettings)
	s.mux.HandleFunc("POST /repo-settings/save/{forge}/{slug...}", s.handleRepoSettingsSave)
	s.mux.HandleFunc("POST /repo-settings/rotate-token/{forge}/{slug...}", s.handleRepoRotateToken)
	s.mux.HandleFunc("POST /repo-settings/delete/{forge}/{slug...}", s.handleRepoDelete)
	s.mux.HandleFunc("GET /repos/{forge}/{slug...}", s.handleRepo)
	s.mux.HandleFunc("GET /uploads/{id}", s.handleUploadPage)
	s.mux.HandleFunc("GET /uploads/{id}/profile", s.handleUploadProfile)
	s.mux.HandleFunc("GET /uploads/{id}/files/{path...}", s.handleSource)
}

// cacheStatic adds cache headers for the embedded assets. URLs carry a
// content-derived ?v=, so long-lived caching is safe across upgrades.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

// staticVersion hashes the embedded static files, changing whenever their
// content changes.
func staticVersion() string {
	h := fnv.New64a()
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write(data)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum64())
}

// HealthTimeout is how long GET /healthz gives its readiness probe before
// calling the instance unhealthy. It is exported because whoever polls the
// endpoint has to outwait it to get an answer rather than a timeout of
// their own — see healthProbeTimeout in cmd/gocov-server.
const HealthTimeout = 2 * time.Second

// handleHealthz reports readiness: 200 when the health probe (typically a
// database ping) succeeds, 503 otherwise.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), HealthTimeout)
		defer cancel()
		if err := s.health(ctx); err != nil {
			s.log.Error("health check", "err", err)
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("unknown page template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.layoutData(r, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
	}
}

// layoutData adds what layout.html reads on every page: the auth state
// behind the open-UI banner and the nav user chip. Page data goes in as
// is; these keys are the layout's.
func (s *Server) layoutData(r *http.Request, data map[string]any) {
	data["AuthOpen"] = !s.authEnabled()
	data["CurrentUser"] = currentUser(r)
	if s.posthog.Configured() {
		data["PostHog"] = s.posthog.view(currentUser(r))
	}
}

// handleNotFound is the catch-all for paths no route claims. Browser
// navigations (a GET that accepts HTML) get the styled 404 page; everything
// else — API clients, other methods — keeps the plain-text 404.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	// A signed-out request can only land here through the public-report
	// pass-through (an unrouted path under /repos/ or /uploads/); answer
	// with the login redirect every other signed-out request gets, so the
	// signed-out response surface stays uniform.
	if s.authEnabled() && currentUser(r) == nil {
		redirectToLogin(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.renderNotFound(w, r)
		return
	}
	http.NotFound(w, r)
}

// renderNotFound writes the styled 404 page with a 404 status. It is the
// HTML-page counterpart to http.NotFound: use it from browser-facing GET
// handlers so a missing (or access-hidden) repo, upload or source view lands
// on the same page as a mistyped URL. The requested path is shown verbatim
// (auto-escaped by html/template).
func (s *Server) renderNotFound(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pages["not-found.html"]
	if !ok {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"RequestedPath": r.URL.Path}
	s.layoutData(r, data)
	// Content-Type must precede WriteHeader; after the status is written the
	// header map is frozen.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render template", "template", "not-found.html", "err", err)
	}
}

// renderPartial executes one named block of a page's template set without
// the layout — the response bodies of htmx poll endpoints.
func (s *Server) renderPartial(w http.ResponseWriter, page, block string, data any) {
	t, ok := s.pages[page]
	if !ok {
		s.log.Error("unknown page template", "template", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, block, data); err != nil {
		s.log.Error("render partial", "template", page, "block", block, "err", err)
	}
}

// shortSHA abbreviates a commit identifier for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// covClass maps a percentage to the badge threshold classes.
func covClass(p float64) string {
	switch {
	case p < 50:
		return "bad"
	case p <= 75:
		return "warn"
	default:
		return "good"
	}
}

// timeAgo renders a compact relative timestamp for tables.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 14*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("2006-01-02")
	}
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) internalError(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}
