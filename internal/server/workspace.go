package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/hosted"
	"github.com/gocov/gocov/internal/store"
)

// Workspace settings page (M3/R3) — the way workspaces are administered:
// token rotation, default branch, one-click forge connection and gate
// defaults. The onboarding/setup page (R4) lives next to it. Both are
// members-only; anyone else 404s (like every other tenant surface, a
// non-member must not learn the workspace exists). Within the workspace
// the role decides: a member reads the pages, an owner — the role the
// forge's own admin/owner role maps to at sign-in — changes things.
// Every mutation and the upload token are owner-only; a member who posts
// anyway gets a 403, since the workspace's existence is no secret to
// them.

// memberWorkspace resolves the {forge}/{prefix} path segments to a
// workspace the signed-in user is a member of, writing a 404 otherwise,
// and returns the user's role in it. With auth off there are no members,
// so the pages do not exist.
func (s *Server) memberWorkspace(w http.ResponseWriter, r *http.Request) (*store.Workspace, store.Role) {
	u := s.signedIn(w, r)
	if u == nil {
		return nil, ""
	}
	ws, err := s.store.WorkspaceByPrefix(r.Context(), r.PathValue("forge"), r.PathValue("prefix"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, ""
	}
	if err != nil {
		s.internalError(w, "loading workspace", err)
		return nil, ""
	}
	role, member, err := s.seat(r.Context(), u, ws)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return nil, ""
	}
	if !member {
		http.NotFound(w, r)
		return nil, ""
	}
	return ws, role
}

// signedIn is the gate on the member-only pages: the signed-in user, or
// nil after writing a 404 — with sign-in off there are no members, so
// the pages do not exist, and a signed-out request must not learn that
// they would.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request) *store.User {
	if !s.authEnabled() {
		http.NotFound(w, r)
		return nil
	}
	u := currentUser(r)
	if u == nil {
		http.NotFound(w, r)
	}
	return u
}

// ownerWorkspace is memberWorkspace for the owner-only routes: a member
// who is not an owner gets a 403 instead of the page.
func (s *Server) ownerWorkspace(w http.ResponseWriter, r *http.Request) *store.Workspace {
	ws, role := s.memberWorkspace(w, r)
	if ws == nil {
		return nil
	}
	if role != store.RoleOwner {
		ownersOnly(w)
		return nil
	}
	return ws
}

// seat reports the user's role in the workspace and whether they are a
// member of it at all.
func (s *Server) seat(ctx context.Context, u *store.User, ws *store.Workspace) (store.Role, bool, error) {
	memberships, err := s.store.ListMembershipsForUser(ctx, u.ID)
	if err != nil {
		return "", false, err
	}
	for _, m := range memberships {
		if m.WorkspaceID == ws.ID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

// ownersOnly answers a member's attempt at an owner-only action. The
// pages hide these controls from members, so this is the answer to a
// hand-built request, not to a click.
func ownersOnly(w http.ResponseWriter) {
	http.Error(w, "only workspace owners can do this — on the forge, that is an admin or owner of the workspace", http.StatusForbidden)
}

// settingsData assembles the template payload for the settings page.
// The upload token never leaves the server either: it is only ever shown
// as newToken right after a rotation.
func (s *Server) settingsData(r *http.Request, ws *store.Workspace, owner bool, newToken, notice, errMsg string) map[string]any {
	data := map[string]any{
		"Workspace":  ws,
		"ForgeLabel": providerLabel(ws.Forge),
		"Owner":      owner,
		"NewToken":   newToken,
		"Notice":     notice,
		"Error":      errMsg,
		// Uploads card: the token is shown to owners on demand (Reveal).
		// It lives on the workspace row in the clear, so this exposes
		// nothing the DB does not already hold; MaskedToken is the
		// default. A member's page carries neither.
		"Token":       "",
		"MaskedToken": "",
		// Self-hosters need GOCOV_SERVER in CI; on the public hosted
		// service the CLI already defaults to it, so the row is dropped.
		"ServerURL":  strings.TrimSuffix(s.baseURL, "/"),
		"ShowServer": strings.TrimSuffix(s.baseURL, "/") != hosted.DefaultServer,
		"GateActive": gateActiveCount(ws.Gate),
	}
	if owner {
		data["Token"] = ws.Token
		data["MaskedToken"] = maskSecret(ws.Token)
	}
	if repos, err := s.workspaceRepos(r, ws); err == nil {
		data["RepoCount"] = len(repos)
	}
	s.addGitHubAppData(r, ws, data)
	s.addGrantData(ws, data)
	s.addReportingState(ws, data)
	return data
}

// addReportingState collapses the per-forge connection flags into the
// single state the consolidated Reporting card renders: whether a
// one-click mechanism exists for this deployment ("available"), whether
// it is currently on/off/broken, and the identity posts carry.
func (s *Server) addReportingState(ws *store.Workspace, data map[string]any) {
	available, _ := data["GitHubApp"].(bool)
	if bb, _ := data["BitbucketConnect"].(bool); bb {
		available = true
	}
	if gl, _ := data["GitLabConnect"].(bool); gl {
		available = true
	}
	state, account := "off", ""
	switch ws.Forge {
	case "github":
		switch {
		case ws.GitHubAppBroken:
			state = "broken"
		case ws.GitHubInstallationID != 0:
			state = "on"
		}
	case "bitbucket":
		account = ws.BitbucketGrantAccount
		switch {
		case ws.BitbucketGrantBroken:
			state = "broken"
		case ws.BitbucketGrantAccount != "":
			state = "on"
		}
	case "gitlab":
		account = ws.GitLabGrantAccount
		switch {
		case ws.GitLabGrantBroken:
			state = "broken"
		case ws.GitLabGrantAccount != "":
			state = "on"
		}
	}
	initial := "?"
	if rs := []rune(account); len(rs) > 0 {
		initial = strings.ToUpper(string(rs[0]))
	}
	data["ReportingAvailable"] = available
	data["ReportingState"] = state
	data["ReportingAccount"] = account
	data["ReportingInitial"] = initial
}

// maskSecret renders a token as its last eight characters behind a run of
// bullets, matching the Reveal control's masked form.
func maskSecret(tok string) string {
	const tail = 8
	if len(tok) <= tail {
		return strings.Repeat("•", len(tok))
	}
	return strings.Repeat("•", 40) + tok[len(tok)-tail:]
}

// gateActiveCount reports how many of the workspace gate rules are set.
func gateActiveCount(g store.Gate) int {
	n := 0
	for _, p := range []*float64{g.MinCoverage, g.MinDiffCoverage, g.MaxCoverageDrop} {
		if p != nil {
			n++
		}
	}
	return n
}

// addGitHubAppData fills the GitHub App connection state shared by the
// settings and setup pages (One-Click Connect P1). Absent when the
// deployment has no App or the workspace is not on GitHub — the pages
// then render exactly as before.
func (s *Server) addGitHubAppData(r *http.Request, ws *store.Workspace, data map[string]any) {
	if s.forges.GitHubApp == nil || ws.Forge != "github" {
		return
	}
	data["GitHubApp"] = true
	data["GitHubAppConnected"] = ws.GitHubInstallationID != 0
	data["GitHubAppBroken"] = ws.GitHubAppBroken
	data["GitHubInstallURL"] = s.forges.InstallURL(r.Context())
}

// handleWorkspacePage implements GET /workspaces/{forge}/{prefix}.
func (s *Server) handleWorkspacePage(w http.ResponseWriter, r *http.Request) {
	ws, role := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	notice := ""
	if r.FormValue("saved") == "1" {
		notice = "Saved."
	}
	if r.FormValue("connected") == "1" {
		switch ws.Forge {
		case "bitbucket":
			notice = "Workspace connected — statuses, PR comments and reports now post as @" +
				ws.BitbucketGrantAccount + "."
		case "gitlab":
			notice = "Workspace connected — statuses and MR comments now post as @" +
				ws.GitLabGrantAccount + "."
		default:
			notice = "GitHub App connected — statuses, PR comments and check runs now post as gocov[bot]."
		}
	}
	s.render(w, r, "workspace.html", s.settingsData(r, ws, role == store.RoleOwner, "", notice, ""))
}

// handleWorkspaceRotate implements POST /workspaces/{forge}/{prefix}/rotate-token.
// The response renders the new token once; the old one is already dead by
// then (single UPDATE, no grace period).
func (s *Server) handleWorkspaceRotate(w http.ResponseWriter, r *http.Request) {
	ws := s.ownerWorkspace(w, r)
	if ws == nil {
		return
	}
	token, err := core.NewToken()
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
	s.render(w, r, "workspace.html", s.settingsData(r, ws, true, token,
		"Token rotated — the previous token no longer works. Update your CI variable.", ""))
}

// handleWorkspaceSettings implements POST /workspaces/{forge}/{prefix}/settings:
// default branch and gate defaults for repos registered from now on.
func (s *Server) handleWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	ws := s.ownerWorkspace(w, r)
	if ws == nil {
		return
	}
	branch := strings.TrimSpace(r.FormValue("default_branch"))
	if branch == "" {
		s.settingsError(w, r, ws, "Default branch cannot be empty.")
		return
	}
	gate, errLabel := parseGateForm(r)
	if errLabel != "" {
		s.settingsError(w, r, ws, errLabel)
		return
	}
	retention := ws.ReportRetentionDays
	if raw := strings.TrimSpace(r.FormValue("report_retention_days")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || !validRetention(v) {
			s.settingsError(w, r, ws, "Report retention must be 90 days, 1 year or forever.")
			return
		}
		retention = v
	}
	ws.DefaultBranch = branch
	ws.Gate = gate
	ws.ReportRetentionDays = retention
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "updating workspace", err)
		return
	}
	http.Redirect(w, r, workspaceURL(ws, "?saved=1"), http.StatusSeeOther)
}

// parseGateForm reads the three coverage-gate percentages from a settings
// form (workspace or repo), returning the assembled gate. A non-empty second
// result is a human-readable validation message; the gate is then zero.
func parseGateForm(r *http.Request) (store.Gate, string) {
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
			return store.Gate{}, f.label + " must be a percentage between 0 and 100."
		}
		*f.field = &v
	}
	return gate, ""
}

// validRetention accepts the retention windows the Defaults selector
// offers: 90 days, 1 year, or 0 (keep forever).
func validRetention(days int) bool {
	switch days {
	case 0, 90, 365:
		return true
	}
	return false
}

// handleWorkspaceDelete implements POST /workspaces/{forge}/{prefix}/delete: it
// removes the workspace and cascades its repos and coverage reports (the
// store does the cascade). Owners only; uploads with the token start
// failing at once and nothing is changed on the forge.
func (s *Server) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	ws := s.ownerWorkspace(w, r)
	if ws == nil {
		return
	}
	if err := s.store.DeleteWorkspace(r.Context(), ws.ID); err != nil {
		s.internalError(w, "deleting workspace", err)
		return
	}
	s.log.Info("workspace deleted", "prefix", ws.Prefix, "user", currentUser(r).DisplayName)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// settingsError re-renders the settings page with a validation message.
// Only an owner's save can fail validation, so the page is an owner's.
func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, ws *store.Workspace, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "workspace.html", s.settingsData(r, ws, true, "", "", msg))
}

// handleWorkspaceSetup implements GET /workspaces/{forge}/{prefix}/setup (M3/R4):
// the onboarding page with the forge-appropriate CI snippet, the upload
// token pre-filled (D6, owners only — a member gets the snippet without
// it) and the waiting-for-first-upload state.
func (s *Server) handleWorkspaceSetup(w http.ResponseWriter, r *http.Request) {
	ws, role := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	data, err := s.setupViewData(r, ws, role == store.RoleOwner)
	if err != nil {
		s.internalError(w, "listing workspace repos", err)
		return
	}
	repos, _ := data["Repos"].([]*store.Repo)
	// Each stage is its own clean panel. Wire up CI (rail 1) is a single
	// card; "I've added these" advances to First upload (rail 2), which
	// polls while waiting and flips to the done state once a repo lands.
	active := 1
	switch {
	case len(repos) > 0:
		active = 2 // First upload — done
	case r.FormValue("awaiting") == "1":
		active = 2 // First upload — waiting
	}
	data["Active"] = active
	data["Forge"] = ws.Forge
	data["Landed"] = active == 2 && len(repos) > 0 && r.FormValue("landed") == "1"
	data["Rail"] = onboardingRail(active, ws.Forge, ws, len(repos) > 0)
	s.render(w, r, "onboarding.html", data)
}

// handleWorkspaceSetupStatus implements GET /workspaces/{forge}/{prefix}/setup/status,
// the htmx poll target that flips the waiting state once the first
// upload has auto-registered a repo.
func (s *Server) handleWorkspaceSetupStatus(w http.ResponseWriter, r *http.Request) {
	ws, role := s.memberWorkspace(w, r)
	if ws == nil {
		return
	}
	data, err := s.setupViewData(r, ws, role == store.RoleOwner)
	if err != nil {
		s.internalError(w, "listing workspace repos", err)
		return
	}
	// The poll only owns the waiting card. Once the first upload lands,
	// reload the whole page so the rail and panel move to the clean
	// First-upload done state instead of stacking it under the CI card.
	// ?landed=1 marks that load as the moment the first upload arrived, so
	// the page can report it once (a later reload of the same state must
	// not count again).
	if repos, _ := data["Repos"].([]*store.Repo); len(repos) > 0 {
		w.Header().Set("HX-Redirect", workspaceURL(ws, "/setup")+"?landed=1")
		return
	}
	s.renderPartial(w, "onboarding.html", "setup-status", data)
}

// workspaceRepos lists the repos under the workspace's prefix.
func (s *Server) workspaceRepos(r *http.Request, ws *store.Workspace) ([]*store.Repo, error) {
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		return nil, err
	}
	var out []*store.Repo
	for _, repo := range repos {
		if ws.Owns(repo) {
			out = append(out, repo)
		}
	}
	return out, nil
}
