package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/bykclk/gocov/internal/store"
)

type repoListItem struct {
	Repo   *store.Repo
	Latest *store.Upload // nil when the repo has no uploads on its default branch
}

// handleIndex implements GET / — the repo list.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		s.internalError(w, "listing repos", err)
		return
	}
	items := make([]repoListItem, 0, len(repos))
	for _, repo := range repos {
		latest, err := s.store.LatestUpload(r.Context(), repo.ID, repo.DefaultBranch)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, "loading latest upload", err)
			return
		}
		items = append(items, repoListItem{Repo: repo, Latest: latest})
	}
	s.render(w, "index.html", map[string]any{"Repos": items})
}

// handleRepo implements GET /repos/{workspace}/{repo} — the upload list.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	repo, err := s.store.RepoBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading repo", err)
		return
	}
	uploads, err := s.store.ListUploads(r.Context(), repo.ID, 100)
	if err != nil {
		s.internalError(w, "listing uploads", err)
		return
	}
	s.render(w, "repo.html", map[string]any{"Repo": repo, "Uploads": uploads})
}

// handleUploadPage implements GET /uploads/{id} — the per-file coverage table.
func (s *Server) handleUploadPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	upload, err := s.store.Upload(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "loading upload", err)
		return
	}
	files, err := s.store.UploadFiles(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "loading upload files", err)
		return
	}
	repo, err := s.store.RepoByID(r.Context(), upload.RepoID)
	if err != nil {
		s.internalError(w, "loading repo for upload", err)
		return
	}
	s.render(w, "upload.html", map[string]any{
		"Upload": upload, "Files": files, "Repo": repo,
	})
}
