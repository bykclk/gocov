package server

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/store"
)

// GitHub App connect flow (One-Click Connect P1/D3). The app's Setup URL
// points at /github/setup: after an install GitHub redirects the
// installing browser here with ?installation_id=…. There is no webhook
// endpoint in v1 — the redirect is the only install signal, and uninstall
// is detected lazily when a token mint fails (upload.go).
//
// Trust model: the query string proves nothing — anyone can put any
// number in it. What the handler believes is (a) the GitHub API's answer,
// via app-JWT auth, about which account the installation lives on, and
// (b) the signed-in user's own membership/forge-workspace state, exactly
// the rules the M3 register flow enforces.

// handleGitHubSetup implements GET /github/setup.
func (s *Server) handleGitHubSetup(w http.ResponseWriter, r *http.Request) {
	if s.forges.GitHubApp == nil || !s.authEnabled() {
		http.NotFound(w, r)
		return
	}
	u := currentUser(r)
	if u == nil {
		// Unreachable behind requireAuth; kept as a guard against future
		// routing changes.
		redirectToLogin(w, r)
		return
	}

	if r.FormValue("setup_action") == "request" {
		// A member without admin rights asked the org owners to install;
		// there is no installation to link yet.
		s.renderConnect(w, r, http.StatusOK, "Install requested",
			"Your request went to the organization owners. Once an owner approves the "+
				"installation, GitHub sends them back here and the workspace gets connected.")
		return
	}

	id, err := strconv.ParseInt(r.FormValue("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderConnect(w, r, http.StatusBadRequest, "Missing installation",
			"This page is where GitHub returns after installing the gocov app; it cannot be "+
				"used on its own. Start from the install page instead.",
			connectAction{URL: s.forges.InstallURL(r.Context()), Label: "Open the install page"})
		return
	}
	login, err := s.forges.GitHubApp.InstallationAccount(r.Context(), id)
	if err != nil {
		s.log.Error("github app installation lookup", "installation", id, "err", err)
		s.renderConnect(w, r, http.StatusBadGateway, "GitHub did not confirm the installation",
			"The installation could not be verified with GitHub. If you just installed the "+
				"app, try reloading this page in a moment.")
		return
	}

	ws, err := s.store.WorkspaceByPrefix(r.Context(), "github", login)
	switch {
	case err == nil:
		s.connectExisting(w, r, u, ws, login, id)
	case errors.Is(err, store.ErrNotFound):
		s.connectNew(w, r, u, login, id)
	default:
		s.internalError(w, "looking up workspace", err)
	}
}

// connectExisting links the installation to an already-registered
// workspace. An owner's seat is required — with the register-flow
// concession (M3/D2): a user whose forge workspace list contains the
// prefix is seated on the spot, in the role the snapshot grants, instead
// of waiting for the next login sync. Installing the App is an org
// admin's move on GitHub too, so an owner arriving here is the normal
// case; a member arriving with an installation most likely became admin
// after their last sign-in, and signing in again refreshes the role.
func (s *Server) connectExisting(w http.ResponseWriter, r *http.Request, u *store.User, ws *store.Workspace, login string, installationID int64) {
	role, member, err := s.seat(r.Context(), u, ws)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return
	}
	if !member {
		if !inForgeWorkspaces(u, login) {
			// Like every tenant surface: a non-member learns nothing
			// beyond what the conflict above already implies.
			s.log.Warn("github setup denied", "user", u.DisplayName, "installation", installationID, "account", login)
			s.renderConnect(w, r, http.StatusForbidden, "Not your workspace",
				"You are not a member of the "+login+" workspace on this server. If "+login+
					" is your organization, it may restrict third-party OAuth apps so gocov can't "+
					"see your membership — allow gocov below, then sign in again.",
				connectAction{URL: githubOrgPolicyURL(login), Label: "Allow gocov on " + login, External: true},
				connectAction{URL: githubReauthURL(installationID), Label: "Sign in again"})
			return
		}
		if err := s.joinWorkspace(r, u, ws); err != nil {
			s.internalError(w, "adding membership", err)
			return
		}
		role = forgeRole(u, login)
	}
	if role != store.RoleOwner {
		s.log.Warn("github setup denied to member", "user", u.DisplayName, "installation", installationID, "account", login)
		s.renderConnect(w, r, http.StatusForbidden, "Owners only",
			"Connecting "+login+" is a workspace owner's move, and your last sign-in listed you as "+
				"a member of "+login+", not an admin. If you have since become one, sign in again "+
				"to refresh your role and come back here.",
			connectAction{URL: githubReauthURL(installationID), Label: "Sign in again"})
		return
	}
	ws.GitHubInstallationID = installationID
	ws.GitHubAppBroken = false
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "linking installation", err)
		return
	}
	s.log.Info("github app connected", "workspace", ws.Prefix, "installation", installationID, "user", u.DisplayName)
	http.Redirect(w, r, workspaceURL(ws, "?connected=1"), http.StatusSeeOther)
}

// connectNew is the install-first path: the account has no workspace here
// yet, so the M3 claim rules apply — only accounts the user's forge list
// vouches for, and, since claiming creates the tenant and mints its
// token, only ones it says the user administers. The workspace is
// registered with the installation already linked, then the user lands
// on the setup page: the same activation moment as the register flow,
// minus the credentials step. Works in hosted and private mode alike;
// the forge snapshot is the only claim gate.
func (s *Server) connectNew(w http.ResponseWriter, r *http.Request, u *store.User, login string, installationID int64) {
	if inForgeWorkspaces(u, login) && forgeRole(u, login) != store.RoleOwner {
		s.log.Warn("github setup claim denied to member", "user", u.DisplayName, "installation", installationID, "account", login)
		s.renderConnect(w, r, http.StatusForbidden, "Owners only",
			"Creating the "+login+" workspace is an organization admin's move, and your last "+
				"sign-in listed you as a member of "+login+", not an admin. If you have since "+
				"become one, sign in again to refresh your role and come back here.",
			connectAction{URL: githubReauthURL(installationID), Label: "Sign in again"})
		return
	}
	if !inForgeWorkspaces(u, login) {
		s.log.Warn("github setup claim denied", "user", u.DisplayName, "installation", installationID, "account", login)
		s.renderConnect(w, r, http.StatusForbidden, "Not your workspace",
			"gocov can see the app installed on "+login+", but your GitHub sign-in didn't list "+
				login+" among your organizations. Most often that means "+login+" restricts "+
				"third-party OAuth apps and hasn't approved gocov's, so it stays hidden no matter "+
				"how many times you sign in. If "+login+" is yours, allow gocov below, then sign "+
				"in again. (If you only just created it, signing in again alone may be enough.)",
			connectAction{URL: githubOrgPolicyURL(login), Label: "Allow gocov on " + login, External: true},
			connectAction{URL: githubReauthURL(installationID), Label: "Sign in again"})
		return
	}
	token, err := core.NewToken()
	if err != nil {
		s.internalError(w, "generating workspace token", err)
		return
	}
	ws := &store.Workspace{
		Forge:                "github",
		Prefix:               login,
		Token:                token,
		DefaultBranch:        "main",
		GitHubInstallationID: installationID,
	}
	if err := s.store.RegisterWorkspace(r.Context(), ws, u.ID); err != nil {
		// A concurrent claim may have won the create race; link to it.
		if existing, lookupErr := s.store.WorkspaceByPrefix(r.Context(), "github", login); lookupErr == nil {
			s.connectExisting(w, r, u, existing, login, installationID)
			return
		}
		s.internalError(w, "registering workspace", err)
		return
	}
	s.log.Info("workspace registered via github app", "prefix", login, "installation", installationID, "user", u.DisplayName)
	http.Redirect(w, r, onboardingReadyURL(ws), http.StatusSeeOther)
}

// githubDisconnect is POST /workspaces/github/{prefix}/disconnect: forget
// the installation link. The installation itself lives on GitHub —
// uninstalling there is the org owner's move; this only stops gocov
// using it and drops resolution back to the credential chain.
func (s *Server) githubDisconnect(w http.ResponseWriter, r *http.Request, ws *store.Workspace) {
	ws.GitHubInstallationID = 0
	ws.GitHubAppBroken = false
	if err := s.store.UpdateWorkspace(r.Context(), ws); err != nil {
		s.internalError(w, "disconnecting github app", err)
		return
	}
	s.log.Info("github app disconnected", "workspace", ws.Prefix, "user", currentUser(r).DisplayName)
	http.Redirect(w, r, workspaceURL(ws, "?saved=1"), http.StatusSeeOther)
}

// inForgeWorkspaces reports whether the login is in the user's stored
// forge workspace snapshot — the same server-side rule the register flow
// enforces (M3/D2).
func inForgeWorkspaces(u *store.User, login string) bool {
	if u.Forge != "github" {
		return false
	}
	return slices.Contains(u.ForgeWorkspaces, login)
}

// renderConnect renders the connect flow's terminal states that have no
// workspace page to land on.
func (s *Server) renderConnect(w http.ResponseWriter, r *http.Request, code int, heading, message string, actions ...connectAction) {
	w.WriteHeader(code)
	s.render(w, r, "connect.html", map[string]any{
		"Heading": heading,
		"Message": message,
		"Actions": actions,
	})
}

// connectAction is one button on a connect terminal page. The first is
// rendered primary; External ones open in a new tab (they leave gocov, e.g.
// a GitHub settings page).
type connectAction struct {
	URL      string
	Label    string
	External bool
}

// githubReauthURL re-runs GitHub sign-in and returns to this same
// installation. A brand-new org is missing from the sign-in snapshot the
// claim gate checks (the OAuth token is dropped at login, D3); re-auth
// refreshes the snapshot, then the setup handler runs again with the same
// installation and the claim goes through — no dead end.
func githubReauthURL(installationID int64) string {
	return "/oauth/github/start?next=" +
		url.QueryEscape("/github/setup?installation_id="+strconv.FormatInt(installationID, 10))
}

// githubOrgPolicyURL is the org's third-party OAuth-app policy page. When an
// installed org is absent from the user's sign-in org list, the usual cause
// is that the org restricts third-party OAuth apps and hasn't approved
// gocov's — so it never appears in GET /user/orgs and no amount of re-auth
// helps. This links an owner straight to the setting to allow gocov.
func githubOrgPolicyURL(login string) string {
	return "https://github.com/organizations/" + url.PathEscape(login) + "/settings/oauth_application_policy"
}
