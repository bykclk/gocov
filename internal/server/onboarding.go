package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gocov/gocov/internal/hosted"
	"github.com/gocov/gocov/internal/store"
)

// The onboarding wizard (unified guided flow). A fixed three-stage rail —
// Workspace, Wire up CI, First upload — carries every forge. The per-forge
// difference lives entirely in the Workspace step (how the workspace is
// created and who reporting posts as), never in the shape of the flow.
//
// Sign-in already happened before onboarding is reachable, so there is no
// separate "connect account" step: for GitHub the workspace is created by
// installing the app (org, repos and bot identity in one approval); for
// Bitbucket/GitLab it is picked from the sign-in membership snapshot.
//
// The Workspace step (rail 0) lives at /onboarding and has three faces —
// install, pick and ready (with the reporting capability card). Wire up CI
// (rail 1) and First upload (rail 2) live at /workspaces/{prefix}/setup.
// Register and the GitHub App install redirect to /onboarding?ws={prefix}
// (the ready state); "Continue to CI" leads to the setup page.

// railStep is one entry in the wizard's step rail.
type railStep struct {
	Num     int
	Label   string
	Subline string // the per-forge / per-step fill
	State   string // "done" | "active" | "todo"
	Href    string // set only for revisitable done steps
}

// onboardingRail builds the three-stage rail for the given active step.
// Labels are identical across forges; only the Workspace subline differs.
func onboardingRail(active int, forge, prefix string, hasRepos bool) []railStep {
	labels := [3]string{"Workspace", "Wire up CI", "First upload"}
	sub := [3]string{"Chosen here", "The upload step", "Push a commit"}
	if forge == "github" {
		// On GitHub the org (and so the workspace) is chosen on GitHub's
		// own install screen, not here.
		sub[0] = "Chosen on GitHub"
	}
	steps := make([]railStep, 3)
	for i := range steps {
		state := "todo"
		switch {
		case i < active:
			state = "done"
		case i == active:
			state = "active"
		}
		steps[i] = railStep{Num: i + 1, Label: labels[i], Subline: sub[i], State: state}
	}
	// The last step completes on its own once the first upload lands.
	if hasRepos {
		steps[2].State = "done"
	}
	if steps[0].State == "done" {
		steps[0].Href = "/onboarding"
	}
	if steps[1].State == "done" && prefix != "" {
		steps[1].Href = workspaceURL(prefix, "/setup")
	}
	return steps
}

// handleOnboarding implements GET /onboarding: the Workspace step (rail 0).
// Hosted-mode and signed-in only, like registration, which it replaces as
// the zero-membership landing.
//
// With ?ws={prefix} it shows the "workspace ready" face for a freshly
// created/selected workspace, including the reporting capability card.
// Otherwise it shows the install prompt (GitHub) or the membership picker
// (Bitbucket/GitLab).
func (s *Server) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	u := s.registerUser(w, r)
	if u == nil {
		return
	}

	if prefix := r.FormValue("ws"); prefix != "" {
		ws, role := s.userWorkspace(r, u, prefix)
		if ws == nil {
			http.Redirect(w, r, "/onboarding", http.StatusFound)
			return
		}
		data := map[string]any{
			"Active":     0,
			"WSState":    "ready",
			"Forge":      ws.Forge,
			"ForgeLabel": providerLabel(ws.Forge),
			"Account":    u.DisplayName,
			"Workspace":  ws,
			"Owner":      role == store.RoleOwner,
			"Rail":       onboardingRail(0, ws.Forge, ws.Prefix, false),
		}
		s.addGitHubAppData(r, ws, data)
		s.addGrantData(ws, data)
		s.reportingState(ws, data)
		if role != store.RoleOwner {
			// Connecting is an owner's move; a member sees the state
			// without the grant button.
			delete(data, "GrantURL")
		}
		s.render(w, r, "onboarding.html", data)
		return
	}

	// GitHub with an App configured creates the workspace by installing the
	// app; the token forges pick from the sign-in snapshot.
	ghApp := s.forges.GitHubApp != nil && u.Forge == "github"
	data := map[string]any{
		"Active":          0,
		"Forge":           u.Forge,
		"ForgeLabel":      providerLabel(u.Forge),
		"Account":         u.DisplayName,
		"MembershipCount": len(u.ForgeWorkspaces),
		"Rail":            onboardingRail(0, u.Forge, "", false),
	}
	if ghApp {
		data["WSState"] = "install"
		data["GitHubInstallURL"] = s.forges.InstallURL(r.Context())
	} else {
		data["WSState"] = "pick"
		rows, err := s.registerRows(r, u)
		if err != nil {
			s.internalError(w, "resolving registrable workspaces", err)
			return
		}
		data["Rows"] = rows
	}
	s.render(w, r, "onboarding.html", data)
}

// userWorkspace returns the workspace the user belongs to with this prefix
// and their role in it, or nil — the membership gate for the ready state.
func (s *Server) userWorkspace(r *http.Request, u *store.User, prefix string) (*store.Workspace, store.Role) {
	ws, err := s.store.WorkspaceByPrefix(r.Context(), prefix)
	if err != nil {
		return nil, ""
	}
	role, member, err := s.seat(r.Context(), u, ws)
	if err != nil || !member {
		return nil, ""
	}
	return ws, role
}

// reportingState fills the reporting capability card's connect state:
// whether reporting is on, the account it posts as, and the one-click grant
// URL. There is no manual-token path — reporting is connect-only.
func (s *Server) reportingState(ws *store.Workspace, data map[string]any) {
	switch ws.Forge {
	case "github":
		data["ReportingConnected"] = ws.GitHubInstallationID != 0
		// GitHubInstallURL is set by addGitHubAppData when an App exists.
		if u, ok := data["GitHubInstallURL"].(string); ok {
			data["GrantURL"] = u
		}
	case "bitbucket":
		data["ReportingConnected"] = ws.BitbucketGrantAccount != ""
		data["GrantAccount"] = ws.BitbucketGrantAccount
		data["GrantURL"] = workspaceURL(ws.Prefix, "/bitbucket/connect") + "?from=onboarding"
	case "gitlab":
		data["ReportingConnected"] = ws.GitLabGrantAccount != ""
		data["GrantAccount"] = ws.GitLabGrantAccount
		data["GrantURL"] = workspaceURL(ws.Prefix, "/gitlab/connect") + "?from=onboarding"
	}
}

// onboardingReadyURL is the redirect target after a workspace is created
// (register claim or GitHub App install): the ready state that shows the
// reporting card before CI.
func onboardingReadyURL(prefix string) string {
	return "/onboarding?ws=" + url.QueryEscape(prefix)
}

// The connect flow (Bitbucket/GitLab grant) returns to wherever it started:
// the onboarding Workspace-ready card, or the settings page. The origin
// rides in the connect state cookie's third field so the callback can route
// back. connectFrom reads it off the start request.
func connectFrom(r *http.Request) string {
	if r.FormValue("from") == "onboarding" {
		return "onboarding"
	}
	return ""
}

// splitConnectState parses the connect state cookie value (state|prefix|from).
func splitConnectState(v string) (state, prefix, from string) {
	parts := strings.SplitN(v, "|", 3)
	if len(parts) > 0 {
		state = parts[0]
	}
	if len(parts) > 1 {
		prefix = parts[1]
	}
	if len(parts) > 2 {
		from = parts[2]
	}
	return state, prefix, from
}

// connectDest is where a completed connect lands: the onboarding
// Workspace-ready state when it started there, else the settings page.
func connectDest(prefix, from string) string {
	if from == "onboarding" {
		return onboardingReadyURL(prefix)
	}
	return workspaceURL(prefix, "") + "?connected=1"
}

// setupViewData assembles the steps 2-3 payload (CI snippet, token and the
// first-upload state) shared by the setup page and its htmx poll partial.
// The token rides only on an owner's page.
func (s *Server) setupViewData(r *http.Request, ws *store.Workspace, owner bool) (map[string]any, error) {
	repos, err := s.workspaceRepos(r, ws)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSuffix(s.baseURL, "/")
	data := map[string]any{
		"Workspace":  ws,
		"ForgeLabel": providerLabel(ws.Forge),
		"BaseURL":    baseURL,
		// When this instance is the public hosted service the CLI already
		// defaults to it, so onboarding drops GOCOV_SERVER (D: ServerImplicit).
		"ServerImplicit": baseURL == hosted.DefaultServer,
		// The GitLab CI/CD Catalog component lives on gitlab.com, and a
		// self-managed instance cannot include it from there: the wizard
		// offers the component only when this server trusts gitlab.com's
		// OIDC issuer, i.e. when its GitLab is gitlab.com.
		"GitLabCatalog": s.gitlabIssuers[gitLabDotComIssuer],
		"Repos":         repos,
		"Owner":         owner,
		"Token":         "",
		"TokenMasked":   "",
		// Tokenless: the CI snippet leads with an OIDC identity token
		// instead of GOCOV_TOKEN. The server only accepts those for a
		// workspace it can verify through its forge connection, which is
		// exactly what the previous step's Connect established.
		"Tokenless": oidcReady(ws),
	}
	if owner {
		data["Token"] = ws.Token
		data["TokenMasked"] = maskToken(ws.Token)
	}
	s.addGitHubAppData(r, ws, data)
	s.addGrantData(ws, data)
	if len(repos) > 0 {
		if fr := s.firstReport(r, repos); fr != nil {
			data["FirstReport"] = fr
		}
		data["ReportsPosted"] = reportsPostedMsg(ws)
	}
	return data, nil
}

// oidcReady reports whether uploads from the workspace's repos can
// authenticate with a forge-minted OIDC identity token instead of the
// upload token: the workspace has to be connected to its forge, since that
// connection is what the server verifies the token's repository against.
func oidcReady(ws *store.Workspace) bool {
	switch ws.Forge {
	case "github":
		return ws.GitHubInstallationID != 0
	case "bitbucket":
		return ws.BitbucketGrantAccount != ""
	case "gitlab":
		return ws.GitLabGrantAccount != ""
	}
	return false
}

// firstReportView is the compact first-upload summary shown on the last
// step. Statements and files are deliberately left for later (the done card
// renders them as dashed placeholders); lines and coverage are enough.
type firstReportView struct {
	Slug        string
	Branch      string
	CommitShort string
	Pct         string
	Covered     int64
	Total       int64
}

// firstReport returns the newest report among the workspace's repos, or nil
// when none has coverage yet.
func (s *Server) firstReport(r *http.Request, repos []*store.Repo) *firstReportView {
	for _, repo := range repos {
		rep, err := s.store.LatestCommitReport(r.Context(), repo.ID, repo.DefaultBranch)
		if err != nil || rep == nil {
			continue
		}
		commit := rep.CommitSHA
		if len(commit) > 7 {
			commit = commit[:7]
		}
		return &firstReportView{
			Slug:        repo.Slug,
			Branch:      rep.Branch,
			CommitShort: commit,
			Pct:         fmt.Sprintf("%.1f", rep.TotalPct),
			Covered:     rep.CoveredStmts,
			Total:       rep.TotalStmts,
		}
	}
	return nil
}

// reportsPostedMsg infers, from the workspace's connect state, whether the
// first upload had a reporting surface to post to — no per-upload log
// needed. Empty means nothing was posted back.
func reportsPostedMsg(ws *store.Workspace) string {
	switch ws.Forge {
	case "github":
		if ws.GitHubInstallationID != 0 {
			return "Commit status posted as gocov[bot]."
		}
	case "bitbucket":
		if ws.BitbucketGrantAccount != "" {
			return "Commit status posted as @" + ws.BitbucketGrantAccount + "."
		}
	case "gitlab":
		if ws.GitLabGrantAccount != "" {
			return "Commit status posted as @" + ws.GitLabGrantAccount + "."
		}
	}
	return ""
}

// maskToken renders the upload token as bullets plus its last four
// characters; the reveal control swaps in the full value client-side.
func maskToken(tok string) string {
	bullets := strings.Repeat("•", 24)
	if len(tok) <= 4 {
		return bullets
	}
	return bullets + tok[len(tok)-4:]
}
