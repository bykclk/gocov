package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/ignore"
	"github.com/gocov/gocov/internal/store"
)

// Repo settings page — the per-repository counterpart to workspace settings:
// coverage gates, base branch, the upload token and repo removal. Like every
// other tenant surface it is members-only; a non-member 404s so the repo's
// existence is never revealed. With auth off there are no members, so the
// pages do not exist. Within the workspace the same split as the
// workspace page applies: members read, owners change.

// memberRepo resolves the {forge}/{slug...} path segments to a repo whose
// owning workspace the signed-in user belongs to, writing a 404
// otherwise. It returns the repo, that workspace (for back-links and
// crumbs) and the user's role there.
func (s *Server) memberRepo(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Workspace, store.Role) {
	u := s.signedIn(w, r)
	if u == nil {
		return nil, nil, ""
	}
	repo, err := s.store.RepoBySlug(r.Context(), r.PathValue("forge"), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, nil, ""
	}
	if err != nil {
		s.internalError(w, "loading repo", err)
		return nil, nil, ""
	}
	memberOf, err := s.store.ListWorkspacesForUser(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return nil, nil, ""
	}
	ws := owningWorkspace(repo, memberOf)
	if ws == nil {
		http.NotFound(w, r)
		return nil, nil, ""
	}
	role, _, err := s.seat(r.Context(), u, ws)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return nil, nil, ""
	}
	return repo, ws, role
}

// ownerRepo is memberRepo for the owner-only routes: a member who is not
// an owner gets a 403 instead.
func (s *Server) ownerRepo(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Workspace) {
	repo, ws, role := s.memberRepo(w, r)
	if repo == nil {
		return nil, nil
	}
	if role != store.RoleOwner {
		ownersOnly(w)
		return nil, nil
	}
	return repo, ws
}

// repoSettingsData assembles the template payload. The upload token lives on
// the repo row in the clear, so exposing it to owners (Reveal) leaks nothing
// the DB does not already hold; MaskedToken is the default rendering. A
// member's page carries neither.
func (s *Server) repoSettingsData(repo *store.Repo, ws *store.Workspace, owner bool, newToken, notice, errMsg string) map[string]any {
	token, masked := "", ""
	if owner {
		token, masked = repo.Token, maskSecret(repo.Token)
	}
	return map[string]any{
		"Repo":              repo,
		"Workspace":         ws,
		"Owner":             owner,
		"Token":             token,
		"MaskedToken":       masked,
		"NewToken":          newToken,
		"Notice":            notice,
		"Error":             errMsg,
		"GateActive":        gateActiveCount(repo.Gate),
		"IgnorePaths":       strings.Join(repo.IgnorePaths, "\n"),
		"BadgeMarkdown":     s.badgeMarkdown(repo),
		"ShowPublicReports": s.publicReportsSwitch(repo),
	}
}

// badgeMarkdown is the copy-paste snippet the repo page and repo settings
// both hand out — one definition, so the two copy buttons can never drift.
// The badge links to the repo page: for a public repo that is a report any
// README reader can open, for a private one the login wall answers as it
// always did.
func (s *Server) badgeMarkdown(repo *store.Repo) string {
	base := strings.TrimSuffix(s.baseURL, "/")
	return fmt.Sprintf("[![coverage](%s%s)](%s%s?ref=badge)", base, badgeURL(repo), base, repoURL(repo))
}

// publicReportsSwitch reports whether the repo-settings "Public reports"
// switch is meaningful for this repo — a repo the forge reports public, on
// an instance that allows public reports. Render and save both consult it,
// so a save can never flip a value the form did not show.
func (s *Server) publicReportsSwitch(repo *store.Repo) bool {
	return s.publicReports && repo.Visibility == store.VisibilityPublic
}

// handleRepoSettings implements GET /repo-settings/{forge}/{slug...}.
func (s *Server) handleRepoSettings(w http.ResponseWriter, r *http.Request) {
	repo, ws, role := s.memberRepo(w, r)
	if repo == nil {
		return
	}
	notice := ""
	if r.FormValue("saved") == "1" {
		notice = "Saved."
	}
	s.render(w, r, "repo-settings.html", s.repoSettingsData(repo, ws, role == store.RoleOwner, "", notice, ""))
}

// handleRepoSettingsSave implements POST /repo-settings/save/{forge}/{slug...}: the base
// branch, coverage gates and ignore patterns for this repository.
func (s *Server) handleRepoSettingsSave(w http.ResponseWriter, r *http.Request) {
	repo, ws := s.ownerRepo(w, r)
	if repo == nil {
		return
	}
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		s.repoSettingsError(w, r, repo, ws, "Base branch cannot be empty.")
		return
	}
	gate, errLabel := parseGateForm(r)
	if errLabel != "" {
		s.repoSettingsError(w, r, repo, ws, errLabel)
		return
	}
	ignorePaths := ignore.Parse(r.FormValue("ignore_paths"))
	if err := ignore.Validate(ignorePaths); err != nil {
		s.repoSettingsError(w, r, repo, ws, "Ignored files not saved: "+err.Error()+".")
		return
	}
	repo.DefaultBranch = branch
	repo.Gate = gate
	repo.IgnorePaths = ignorePaths
	// The "Public reports" switch only renders (and may only change) where
	// it is meaningful; a private repo's save must not flip the stored
	// value just because the form had no checkbox to send.
	if s.publicReportsSwitch(repo) {
		repo.PublicReportsDisabled = r.FormValue("public_reports") == ""
	}
	if err := s.store.UpdateRepo(r.Context(), repo); err != nil {
		s.internalError(w, "updating repo", err)
		return
	}
	http.Redirect(w, r, repoSettingsURL(repo, "")+"?saved=1", http.StatusSeeOther)
}

// handleRepoRotateToken implements POST /repo-settings/rotate-token/{forge}/{slug...}.
// The new token is shown once; the old one is dead by the time the page
// renders (single UPDATE, no grace period).
func (s *Server) handleRepoRotateToken(w http.ResponseWriter, r *http.Request) {
	repo, ws := s.ownerRepo(w, r)
	if repo == nil {
		return
	}
	token, err := core.NewToken()
	if err != nil {
		s.internalError(w, "generating repo token", err)
		return
	}
	repo.Token = token
	if err := s.store.UpdateRepo(r.Context(), repo); err != nil {
		s.internalError(w, "rotating repo token", err)
		return
	}
	s.log.Info("repo token rotated", "slug", repo.Slug, "user", currentUser(r).DisplayName)
	s.render(w, r, "repo-settings.html", s.repoSettingsData(repo, ws, true, token,
		"Token rotated — the previous token no longer works. Update your CI variable.", ""))
}

// handleRepoDelete implements POST /repo-settings/delete/{forge}/{slug...}: it removes the
// repo and cascades its uploads and reports (the store does the cascade).
// Uploads with the token start failing at once; nothing is changed on the forge.
func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	repo, ws := s.ownerRepo(w, r)
	if repo == nil {
		return
	}
	if err := s.store.DeleteRepo(r.Context(), repo.ID); err != nil {
		s.internalError(w, "deleting repo", err)
		return
	}
	s.log.Info("repo deleted", "slug", repo.Slug, "user", currentUser(r).DisplayName)
	http.Redirect(w, r, workspaceURL(ws, ""), http.StatusSeeOther)
}

// repoSettingsError re-renders the settings page with a validation message.
// Only an owner's save can fail validation, so the page is an owner's.
func (s *Server) repoSettingsError(w http.ResponseWriter, r *http.Request, repo *store.Repo, ws *store.Workspace, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "repo-settings.html", s.repoSettingsData(repo, ws, true, "", "", msg))
}
