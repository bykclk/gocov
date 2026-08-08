package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bykclk/gocov/internal/store"
)

// Workspace settings page (M3/R3) — CLI parity in the UI: token rotation,
// default branch, workspace-level forge credentials (D4) and gate
// defaults. The onboarding/setup page (R4) lives next to it. Both are
// members-only; anyone else 404s (like every other tenant surface, a
// non-member must not learn the workspace exists).

// memberWorkspace resolves the {prefix} path segment to a workspace the
// signed-in user is a member of, writing a 404 otherwise. With auth off
// there are no members, so the pages do not exist.
func (s *Server) memberWorkspace(w http.ResponseWriter, r *http.Request) *store.Workspace {
	if !s.authEnabled() {
		http.NotFound(w, r)
		return nil
	}
	u := currentUser(r)
	if u == nil {
		http.NotFound(w, r)
		return nil
	}
	prefix := r.PathValue("prefix")
	memberOf, err := s.store.ListWorkspacesForUser(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return nil
	}
	for _, ws := range memberOf {
		if ws.Prefix == prefix {
			return ws
		}
	}
	http.NotFound(w, r)
	return nil
}

// settingsData assembles the template payload for the settings page.
// Stored secrets never leave the server: credentials render as a
// configured/not-configured state only (D4), and the upload token is
// only ever shown as newToken right after a rotation.
func settingsData(ws *store.Workspace, newToken, notice, errMsg string) map[string]any {
	return map[string]any{
		"Workspace":       ws,
		"ForgeLabel":      providerLabel(ws.Forge),
		"CredsConfigured": len(ws.ForgeCredentials) > 0,
		"NewToken":        newToken,
		"Notice":          notice,
		"Error":           errMsg,
	}
}

// handleWorkspacePage implements GET /workspaces/{prefix}.
func (s *Server) handleWorkspacePage(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	notice := ""
	if r.FormValue("saved") == "1" {
		notice = "Saved."
	}
	s.render(w, r, "workspace.html", settingsData(ws, "", notice, ""))
}

// handleWorkspaceRotate implements POST /workspaces/{prefix}/rotate-token.
// The response renders the new token once; the old one is already dead by
// then (single UPDATE, no grace period — same semantics as the CLI).
func (s *Server) handleWorkspaceRotate(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	token, err := newToken()
	if err != nil {
		s.internalError(w, "generating workspace token", err)
		return
	}
	ws.Token = token
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "rotating workspace token", err)
		return
	}
	s.log.Info("workspace token rotated", "prefix", ws.Prefix, "user", currentUser(r).DisplayName)
	s.render(w, r, "workspace.html", settingsData(ws, token,
		"Token rotated — the previous token no longer works. Update your CI variable.", ""))
}

// handleWorkspaceSettings implements POST /workspaces/{prefix}/settings:
// default branch and gate defaults, mirroring `workspace update`.
func (s *Server) handleWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		s.settingsError(w, r, ws, "Default branch cannot be empty.")
		return
	}
	gate := store.Gate{}
	for _, f := range []struct {
		name  string
		field **float64
		label string
	}{
		{"min_coverage", &gate.MinCoverage, "Min coverage"},
		{"min_diff_coverage", &gate.MinDiffCoverage, "Min diff coverage"},
		{"max_coverage_drop", &gate.MaxCoverageDrop, "Max coverage drop"},
	} {
		raw := strings.TrimSpace(r.FormValue(f.name))
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 100 {
			s.settingsError(w, r, ws, f.label+" must be a percentage between 0 and 100.")
			return
		}
		*f.field = &v
	}
	ws.DefaultBranch = branch
	ws.Gate = gate
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "updating workspace", err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Prefix+"?saved=1", http.StatusSeeOther)
}

// handleWorkspaceCredentials implements POST /workspaces/{prefix}/credentials
// (D4): set/replace or clear the workspace-level bot credential. Values
// are write-only — nothing stored is ever rendered back.
func (s *Server) handleWorkspaceCredentials(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	switch r.FormValue("action") {
	case "clear":
		ws.ForgeCredentials = nil
	default:
		creds, errMsg := credentialsFromForm(ws.Forge, r)
		if errMsg != "" {
			s.settingsError(w, r, ws, errMsg)
			return
		}
		ws.ForgeCredentials = creds
	}
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "updating workspace credentials", err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Prefix+"?saved=1", http.StatusSeeOther)
}

// credentialsFromForm validates the forge-specific credential fields,
// mirroring the CLI's pairing rules (`-bb-username`/`-bb-app-password`
// vs `-gh-token`).
func credentialsFromForm(forgeName string, r *http.Request) (map[string]string, string) {
	switch forgeName {
	case "github":
		token := strings.TrimSpace(r.FormValue("token"))
		if token == "" {
			return nil, "A GitHub access token is required."
		}
		return map[string]string{"token": token}, ""
	default: // bitbucket
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("app_password")
		if username == "" || password == "" {
			return nil, "Username and app password must both be set."
		}
		return map[string]string{"username": username, "app_password": password}, ""
	}
}

// settingsError re-renders the settings page with a validation message.
func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, ws *store.Workspace, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "workspace.html", settingsData(ws, "", "", msg))
}

// handleWorkspaceSetup implements GET /workspaces/{prefix}/setup (M3/R4):
// the onboarding page with the forge-appropriate CI snippet, the upload
// token pre-filled (D6) and the waiting-for-first-upload state.
func (s *Server) handleWorkspaceSetup(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	repos, err := s.workspaceRepos(r, ws)
	if err != nil {
		s.internalError(w, "listing workspace repos", err)
		return
	}
	s.render(w, r, "setup.html", map[string]any{
		"Workspace":  ws,
		"ForgeLabel": providerLabel(ws.Forge),
		"BaseURL":    strings.TrimSuffix(s.baseURL, "/"),
		"Repos":      repos,
	})
}

// handleWorkspaceSetupStatus implements GET /workspaces/{prefix}/setup/status,
// the htmx poll target that flips the waiting state once the first
// upload has auto-registered a repo.
func (s *Server) handleWorkspaceSetupStatus(w http.ResponseWriter, r *http.Request) {
	ws := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	repos, err := s.workspaceRepos(r, ws)
	if err != nil {
		s.internalError(w, "listing workspace repos", err)
		return
	}
	s.renderPartial(w, "setup.html", "setup-status", map[string]any{
		"Workspace": ws,
		"Repos":     repos,
	})
}

// workspaceRepos lists the repos under the workspace's prefix.
func (s *Server) workspaceRepos(r *http.Request, ws *store.Workspace) ([]*store.Repo, error) {
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		return nil, err
	}
	var out []*store.Repo
	for _, repo := range repos {
		if strings.HasPrefix(repo.Slug, ws.Prefix+"/") {
			out = append(out, repo)
		}
	}
	return out, nil
}
