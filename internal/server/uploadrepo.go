// Answering "which repo is this upload for, and may this token write to
// it?" — bearer token lookup, the repo the request names, and the
// automatic registration a workspace token performs for a repo seen for
// the first time.
package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/store"
)

// lookupUploadToken authenticates the Bearer token as either a per-repo
// token or a workspace token, writing the error response itself. Runs
// before the request body is parsed.
func (s *Server) lookupUploadToken(w http.ResponseWriter, r *http.Request, token string) (*store.Repo, *store.Workspace, bool) {
	ctx := r.Context()
	repo, err := s.store.RepoByToken(ctx, token)
	if err == nil {
		return repo, nil, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "looking up token", err)
		return nil, nil, false
	}
	ws, err := s.store.WorkspaceByToken(ctx, token)
	if err == nil {
		return nil, ws, true
	}
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid token")
		return nil, nil, false
	}
	s.internalError(w, "looking up workspace token", err)
	return nil, nil, false
}

// resolveUploadRepo maps the authenticated token to the target repo,
// writing the error response itself on failure. Workspace tokens require
// the repo slug, must match the workspace prefix, and register unknown
// repos on the fly.
func (s *Server) resolveUploadRepo(w http.ResponseWriter, r *http.Request, repo *store.Repo, ws *store.Workspace, slug string) (_ *store.Repo, created, ok bool) {
	ctx := r.Context()
	if repo != nil {
		if slug != "" && slug != repo.Slug {
			httpError(w, http.StatusForbidden, "token is for repo %q, not %q", repo.Slug, slug)
			return nil, false, false
		}
		return repo, false, true
	}

	if slug == "" {
		httpError(w, http.StatusBadRequest, "workspace tokens require the repo field")
		return nil, false, false
	}
	name, matched := strings.CutPrefix(slug, ws.Prefix+"/")
	if !matched {
		httpError(w, http.StatusForbidden, "token is for workspace %q, not %q", ws.Prefix, slug)
		return nil, false, false
	}
	if !core.ValidRepoName(ws.Forge, name) {
		httpError(w, http.StatusBadRequest, "invalid repo name %q under workspace %q", slug, ws.Prefix)
		return nil, false, false
	}

	repo, err := s.store.RepoBySlug(ctx, ws.Forge, slug)
	if err == nil {
		return repo, false, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "looking up repo", err)
		return nil, false, false
	}
	repo, err = s.pipeline.RegisterRepo(ctx, ws, slug)
	if errors.Is(err, forge.ErrRepoNotFound) {
		httpError(w, http.StatusNotFound, "repo %q not found on %s", slug, ws.Forge)
		return nil, false, false
	}
	if err != nil {
		s.internalError(w, "auto-registering repo", err)
		return nil, false, false
	}
	return repo, true, true
}
