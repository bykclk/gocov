package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gocov/gocov/internal/store"
)

// Workspace-connect grants for Bitbucket and GitLab (One-Click Connect
// P2/D6/D7). An owner clicks Connect on the settings page, consents once
// on the forge, and the workspace from then on acts through that grant:
// statuses, PR comments, reports, diff and source fetch — no manual
// credentials. The grant's refresh token lives on the workspace row,
// encrypted at rest; posts visibly carry the connecting account (D8).
//
// Both forges rotate refresh tokens on every use: each refresh
// invalidates the stored token and returns a new one. The refresh path
// (core.Forges) serializes per workspace and persists the rotated token
// before the access token is used; the handlers here only start the
// consent, take the code back, and forget the grant on disconnect.
//
// The two forges differ in nothing but the connector, the state cookie
// that binds their consent redirect, and the workspace columns the grant
// lives in — connectGrant carries those, and one set of handlers serves
// both.
type connectGrant struct {
	// forge names the forge everywhere: store value, route segment, log
	// prefix and connector lookup.
	forge string
	// label is the forge as people see it.
	label string
	// cookie binds the consent redirect to the browser that started it,
	// and carries the workspace prefix being connected.
	cookie string
	// dataKey is the template flag saying this deployment offers the
	// grant for the workspace's forge; the reporting card keys off it.
	dataKey string
	// set writes the grant's columns on the workspace row.
	set func(st store.Store, ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
}

// The connect state cookies, one per forge so an in-flight consent on
// one forge cannot be mistaken for the other's.
const (
	connectStateCookie   = "gocov_connect_state"
	glConnectStateCookie = "gocov_gl_connect_state"
)

// connectGrants lists the grant-backed forges. Whether a deployment
// actually offers a grant is the connector's presence in core.Forges.
var connectGrants = []*connectGrant{
	{forge: "bitbucket", label: "Bitbucket", cookie: connectStateCookie,
		dataKey: "BitbucketConnect", set: store.Store.SetWorkspaceBitbucketGrant},
	{forge: "gitlab", label: "GitLab", cookie: glConnectStateCookie,
		dataKey: "GitLabConnect", set: store.Store.SetWorkspaceGitLabGrant},
}

// connectGrantFor returns the grant description for a forge, nil for a
// forge that connects some other way (or not at all).
func connectGrantFor(forgeName string) *connectGrant {
	for _, g := range connectGrants {
		if g.forge == forgeName {
			return g
		}
	}
	return nil
}

// handleConnect implements GET /workspaces/{forge}/{prefix}/connect: the
// start of the connect grant — state cookie, then the forge's consent
// page. A GitHub workspace connects by installing the App instead, so it
// has no grant to start and the page does not exist.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	// The path's forge is the workspace's forge (that is how the row is
	// looked up), so whether a grant exists is known before any query.
	g := connectGrantFor(r.PathValue("forge"))
	if g == nil {
		http.NotFound(w, r)
		return
	}
	connector := s.forges.Connector(g.forge)
	if connector == nil {
		http.NotFound(w, r)
		return
	}
	ws := s.ownerWorkspace(w, r)
	if ws == nil {
		return
	}
	state, err := newState()
	if err != nil {
		s.internalError(w, "generating connect state", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     g.cookie,
		Value:    state + "|" + ws.Prefix + "|" + connectFrom(r),
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, connector.AuthorizeURL(state, s.redirectURI(g.forge)), http.StatusFound)
}

// connectCallback reports whether the sign-in callback request is
// really a returning connect consent, and handles it when so. The
// forges require the redirect URI to match the configured callback
// exactly, so the connect grant shares the sign-in callback and the
// connect state cookie is what tells the two flows apart.
func (s *Server) connectCallback(g *connectGrant, w http.ResponseWriter, r *http.Request) bool {
	connector := s.forges.Connector(g.forge)
	if connector == nil {
		return false
	}
	c, err := r.Cookie(g.cookie)
	if err != nil {
		return false
	}
	state, prefix, from := splitConnectState(c.Value)
	if state == "" || r.FormValue("state") != state {
		// Not this flow's redirect (a plain sign-in, or garbage); the
		// stale cookie stays until it expires or a connect finishes.
		return false
	}
	clearCookie(w, g.cookie, s.secureCookies)
	code := r.FormValue("code")
	if r.FormValue("error") != "" || code == "" || prefix == "" {
		s.log.Warn(g.forge+" connect callback rejected", "forge_error", r.FormValue("error"))
		s.renderConnect(w, r, http.StatusBadRequest, "Connect failed",
			"The consent redirect could not be validated. Start again from the workspace settings page.")
		return true
	}
	u := s.sessionUser(r)
	if u == nil {
		s.renderConnect(w, r, http.StatusForbidden, "Sign in first",
			"Your gocov session expired during the consent. Sign in and start the connect again "+
				"from the workspace settings page.")
		return true
	}
	ws, err := s.store.WorkspaceByPrefix(r.Context(), g.forge, prefix)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return true
		}
		s.internalError(w, "looking up workspace", err)
		return true
	}
	role, member, err := s.seat(r.Context(), u, ws)
	if err != nil {
		s.internalError(w, "listing memberships", err)
		return true
	}
	if !member {
		http.NotFound(w, r)
		return true
	}
	if role != store.RoleOwner {
		// The consent was started by an owner's session; being demoted
		// (or handing the redirect to a member) in between ends it here.
		ownersOnly(w)
		return true
	}

	grant, err := connector.Exchange(r.Context(), code, s.redirectURI(g.forge))
	if err != nil {
		s.log.Error(g.forge+" connect exchange", "workspace", ws.Prefix, "err", err)
		s.renderConnect(w, r, http.StatusBadGateway, g.label+" did not confirm the grant",
			"The authorization could not be completed with "+g.label+". Start again from the "+
				"workspace settings page.")
		return true
	}
	if err := g.set(s.store, r.Context(), ws.ID, grant.Account, grant.RefreshToken, false); err != nil {
		s.internalError(w, "storing workspace grant", err)
		return true
	}
	s.forges.CacheGrantToken(g.forge, ws.ID, grant.AccessToken, grant.TTL)
	s.log.Info(g.forge+" workspace connected", "workspace", ws.Prefix, "account", grant.Account, "user", u.DisplayName)
	http.Redirect(w, r, connectDest(ws, from), http.StatusSeeOther)
	return true
}

// handleDisconnect implements POST /workspaces/{forge}/{prefix}/disconnect:
// forget the grant. The consent itself lives on the forge (the account's
// authorized-applications page); this only stops gocov using it and
// drops resolution back to the credential chain. A GitHub workspace's
// connection is its App installation link; forgetting that is
// githubDisconnect's job.
func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	ws := s.ownerWorkspace(w, r)
	if ws == nil {
		return
	}
	if ws.Forge == "github" {
		s.githubDisconnect(w, r, ws)
		return
	}
	g := connectGrantFor(ws.Forge)
	if g == nil {
		http.NotFound(w, r)
		return
	}
	if err := g.set(s.store, r.Context(), ws.ID, "", "", false); err != nil {
		s.internalError(w, "disconnecting "+g.forge+" grant", err)
		return
	}
	s.forges.DropGrantToken(g.forge, ws.ID)
	s.log.Info(g.forge+" workspace disconnected", "workspace", ws.Prefix, "user", currentUser(r).DisplayName)
	http.Redirect(w, r, workspaceURL(ws, "?saved=1"), http.StatusSeeOther)
}

// addGrantData flags, for the settings and setup pages, that this
// deployment offers the connect grant for the workspace's forge. The
// connection's state itself is folded into the reporting card by
// addReportingState.
func (s *Server) addGrantData(ws *store.Workspace, data map[string]any) {
	if g := connectGrantFor(ws.Forge); g != nil && s.forges.Connector(g.forge) != nil {
		data[g.dataKey] = true
	}
}
