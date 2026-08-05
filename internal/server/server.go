// Package server implements the gocov HTTP API, badge endpoint and web UI.
package server

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bykclk/gocov/internal/auth"
	"github.com/bykclk/gocov/internal/blobstore"
	"github.com/bykclk/gocov/internal/diffcov"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
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
	Forges  map[string]forge.Factory  // by forge name, e.g. "bitbucket"
	BaseURL string                    // public URL of this server, for links in build statuses
	Logger  *slog.Logger
	// Health is probed by GET /healthz (e.g. a database ping).
	// When nil, /healthz always reports healthy.
	Health func(ctx context.Context) error
	// DefaultForgeCredentials maps a forge name to fallback credentials
	// (e.g. a global bot account) used for repos that have none of their
	// own. Per-repo credentials always take precedence.
	DefaultForgeCredentials map[string]map[string]string
	// Auth enables web UI sign-in. Nil keeps the UI open (with a banner
	// explaining how to enable sign-in); the upload API, badges and health
	// checks are unaffected either way.
	Auth auth.Provider
	// AllowedWorkspaces overrides the derived "tracked workspaces" set
	// that gates who may sign in. Empty means derive from the store.
	AllowedWorkspaces []string
}

// Server is the gocov HTTP server.
type Server struct {
	store        store.Store
	blobs        blobstore.Store
	parsers      map[string]profile.Parser
	forges       map[string]forge.Factory
	baseURL      string
	log          *slog.Logger
	pages        map[string]*template.Template
	mux          *http.ServeMux
	handler      http.Handler // mux wrapped in the auth middleware
	health       func(ctx context.Context) error
	defaultCreds map[string]map[string]string

	auth              auth.Provider
	allowedWorkspaces []string
	// secureCookies marks auth cookies Secure when the public base URL is
	// https (the UI is then served through TLS or a terminating proxy).
	secureCookies bool
}

// New builds a Server; panics only on programmer error (bad templates).
func New(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	assetVer := staticVersion()
	funcs := template.FuncMap{
		"pct": func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
		"short": func(sha string) string {
			if len(sha) > 12 {
				return sha[:12]
			}
			return sha
		},
		"ranges":   diffcov.Ranges,
		"covclass": covClass,
		"timeago":  timeAgo,
		// asset appends a content-derived version so browsers refetch
		// embedded assets after a server upgrade despite long cache TTLs.
		"asset": func(path string) string { return path + "?v=" + assetVer },
	}
	// Every page is its own template set sharing the layout and partials,
	// so pages can define "content" without colliding.
	pages := map[string]*template.Template{}
	for _, name := range []string{"index.html", "repo.html", "upload.html", "source.html", "login.html"} {
		pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(templatesFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name))
	}

	s := &Server{
		store:        cfg.Store,
		blobs:        cfg.Blobs,
		parsers:      cfg.Parsers,
		forges:       cfg.Forges,
		baseURL:      cfg.BaseURL,
		log:          log,
		pages:        pages,
		mux:          http.NewServeMux(),
		health:       cfg.Health,
		defaultCreds: cfg.DefaultForgeCredentials,

		auth:              cfg.Auth,
		allowedWorkspaces: cfg.AllowedWorkspaces,
		secureCookies:     strings.HasPrefix(cfg.BaseURL, "https://"),
	}
	s.routes()
	s.handler = s.requireAuth(s.mux)
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v1/upload", s.handleUpload)
	s.mux.HandleFunc("GET /badge/{slug...}", s.handleBadge)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /static/", cacheStatic(http.FileServerFS(staticFS)))
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("GET /oauth/bitbucket/start", s.handleOAuthStart)
	s.mux.HandleFunc("GET /oauth/bitbucket/callback", s.handleOAuthCallback)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /repos/{slug...}", s.handleRepo)
	s.mux.HandleFunc("GET /uploads/{id}", s.handleUploadPage)
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

// handleHealthz reports readiness: 200 when the health probe (typically a
// database ping) succeeds, 503 otherwise.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
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
	// Layout-level auth state: the open-UI banner and the nav user chip.
	data["AuthOpen"] = s.auth == nil
	data["CurrentUser"] = currentUser(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
	}
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
