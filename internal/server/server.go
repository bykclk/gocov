// Package server implements the gocov HTTP API, badge endpoint and web UI.
package server

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/bykclk/gocov/internal/blobstore"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Config wires the server's dependencies. All fields are required except
// Logger and BaseURL.
type Config struct {
	Store   store.Store
	Blobs   blobstore.Store
	Parsers map[string]profile.Parser // by format name, e.g. "go"
	Forges  map[string]forge.Factory  // by forge name, e.g. "bitbucket"
	BaseURL string                    // public URL of this server, for links in build statuses
	Logger  *slog.Logger
}

// Server is the gocov HTTP server.
type Server struct {
	store   store.Store
	blobs   blobstore.Store
	parsers map[string]profile.Parser
	forges  map[string]forge.Factory
	baseURL string
	log     *slog.Logger
	tmpl    *template.Template
	mux     *http.ServeMux
}

// New builds a Server; panics only on programmer error (bad templates).
func New(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	funcs := template.FuncMap{
		"pct": func(v float64) string { return fmt.Sprintf("%.1f%%", v) },
		"short": func(sha string) string {
			if len(sha) > 12 {
				return sha[:12]
			}
			return sha
		},
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))

	s := &Server{
		store:   cfg.Store,
		blobs:   cfg.Blobs,
		parsers: cfg.Parsers,
		forges:  cfg.Forges,
		baseURL: cfg.BaseURL,
		log:     log,
		tmpl:    tmpl,
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v1/upload", s.handleUpload)
	s.mux.HandleFunc("GET /badge/{slug...}", s.handleBadge)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /repos/{slug...}", s.handleRepo)
	s.mux.HandleFunc("GET /uploads/{id}", s.handleUploadPage)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
	}
}
