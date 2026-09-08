// Tokenless OIDC uploads: authenticating an upload that carries no bearer
// token and no fork-PR run claim, but a forge-minted OIDC identity token.
// A repo owner's own CI (push, same-repo PR) asks the forge for a
// short-lived JWT scoped to gocov's audience and sends it in place of the
// pasted GOCOV_TOKEN; this file verifies that JWT (internal/oidc has the
// protocol) and maps it to a tracked repo. Unlike a fork-PR tokenless
// upload (tokenless.go), the result is a fully verified upload — the forge
// signed the token, so there is no "unverified" badge.
//
// How the token names its repo differs per forge, so mapping does too:
// GitHub's token carries the slug directly ("repository"), while
// Bitbucket's carries an opaque UUID — resolved against the tracked repo's
// own forge-reported id, so the untrusted slug the request also sends can
// only ever agree with the signed identity, never override it.
//
// A repo seen for the first time is registered on the spot, the way a
// workspace token's first upload registers it (uploadrepo.go): the forge
// signed the repo's identity, which is a stronger claim than holding the
// workspace token. What OIDC cannot do is create the workspace — that
// stays an owner's move — so a slug under no registered workspace is
// refused, and the CI log says which workspace to register.
package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/oidc"
	"github.com/gocov/gocov/internal/store"
)

// gitHubActionsIssuer is the GitHub Actions OIDC issuer. Its identity tokens
// carry a "repository" claim (owner/name) that names the repo a run belongs
// to — the claim we map to a tracked repo.
const gitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// gitLabDotComIssuer is gitlab.com's OIDC issuer, and the default trusted
// GitLab issuer. GitLab CI ID tokens carry a "project_path" claim
// (group/project) — the slug we map to a tracked repo. A self-managed
// GitLab issues under its own URL; setting GOCOV_OIDC_ISSUERS
// (server.Config.OIDCIssuers) replaces this default with those instances.
const gitLabDotComIssuer = "https://gitlab.com"

// The fixed parts of a Bitbucket Pipelines OIDC issuer URL. Only the
// workspace segment varies, so the issuer is validated by shape and then
// rebuilt from a trusted workspace slug rather than trusted verbatim.
const (
	bitbucketIssuerPrefix = "https://api.bitbucket.org/2.0/workspaces/"
	bitbucketIssuerSuffix = "/pipelines-config/identity/oidc"
)

// bitbucketIssuerPath captures the single workspace path segment of a
// Bitbucket issuer.
var bitbucketIssuerPath = regexp.MustCompile(`^/2\.0/workspaces/([^/]+)/pipelines-config/identity/oidc$`)

// bitbucketIssuerWorkspace validates a Bitbucket Pipelines OIDC issuer by
// shape and returns the workspace slug it names. The scheme and host are
// pinned to Bitbucket's and the workspace is bounded to one path segment;
// no credentials, query, or fragment (a real issuer carries none). ok=false
// means it is not a Bitbucket issuer at all.
func bitbucketIssuerWorkspace(issuer string) (workspace string, ok bool) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host != "api.bitbucket.org" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	m := bitbucketIssuerPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// bitbucketIssuerMatch reports whether the issuer is a Bitbucket Pipelines
// OIDC issuer — used only to route a verified token to the bitbucket claim
// mapping, never to decide what to fetch.
func bitbucketIssuerMatch(issuer string) bool {
	_, ok := bitbucketIssuerWorkspace(issuer)
	return ok
}

// bitbucketIssuerResolver admits a Bitbucket OIDC issuer only when its
// workspace is a tracked Bitbucket workspace, and returns the discovery URL
// rebuilt from that tracked workspace's stored slug — never the token's own
// issuer string. This does two things at once: it keeps the fetch target
// off attacker control (the URL is literals plus a slug read back from the
// store), and it bounds which issuers can trigger an outbound fetch to the
// workspaces this deployment actually tracks, so an unauthenticated caller
// cannot cycle workspace names to drive requests at Bitbucket.
func bitbucketIssuerResolver(st store.Store) func(context.Context, string) (string, bool) {
	return func(ctx context.Context, issuer string) (string, bool) {
		workspace, ok := bitbucketIssuerWorkspace(issuer)
		if !ok {
			return "", false
		}
		ws, err := st.WorkspaceByPrefix(ctx, "bitbucket", workspace)
		if err != nil {
			return "", false
		}
		return bitbucketIssuerPrefix + ws.Prefix + bitbucketIssuerSuffix, true
	}
}

// oidcForge names the forge a verified token's issuer belongs to, or "" for
// an issuer with no mapping (which the allowlist and matcher keep out). The
// GitLab set is per-instance — gitlab.com plus any operator-configured
// self-managed issuers — so it lives on the server, not in a constant.
func (s *Server) oidcForge(issuer string) string {
	switch {
	case issuer == gitHubActionsIssuer:
		return "github"
	case bitbucketIssuerMatch(issuer):
		return "bitbucket"
	case s.gitlabIssuers[issuer]:
		return "gitlab"
	}
	return ""
}

// authOIDC authenticates an upload carrying a forge OIDC token in the
// oidc_token form field, writing the error response itself; a false second
// return means the response is written. The body is already parsed by the
// caller. Rejections are explicit, like the fork-PR path: the uploader
// prints our reason into the CI log, and silent failure is the competitor
// behavior this feature exists to not have.
func (s *Server) authOIDC(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	ctx := r.Context()
	if s.oidc == nil {
		httpError(w, http.StatusForbidden, "OIDC uploads are not available: this server has no public URL configured")
		return nil, false
	}
	tok, err := s.oidc.Verify(ctx, r.FormValue("oidc_token"))
	switch {
	case err == nil:
	case errors.Is(err, oidc.ErrUnknownIssuer):
		httpError(w, http.StatusForbidden, "oidc_unknown_issuer: the OIDC token's issuer is not one this server trusts")
		return nil, false
	case errors.Is(err, oidc.ErrBadAudience):
		httpError(w, http.StatusForbidden, "oidc_bad_audience: the OIDC token was not minted for this server; set the token audience to %s", s.baseURL)
		return nil, false
	case errors.Is(err, oidc.ErrInvalidToken):
		httpError(w, http.StatusUnauthorized, "oidc_invalid_token: the OIDC token is not valid (%v)", err)
		return nil, false
	default:
		s.log.Error("oidc verification", "err", err)
		httpError(w, http.StatusBadGateway, "could not verify the OIDC token with the forge; retrying may help")
		return nil, false
	}

	switch s.oidcForge(tok.Issuer) {
	case "github":
		// GitHub names the repo by slug in the token itself.
		return s.oidcResolveBySlug(w, r, tok.Claim("repository"), "github")
	case "gitlab":
		// GitLab names the project by its full path — the slug.
		return s.oidcResolveBySlug(w, r, tok.Claim("project_path"), "gitlab")
	case "bitbucket":
		return s.oidcResolveBitbucket(w, r, tok)
	default:
		httpError(w, http.StatusForbidden, "oidc_invalid_token: unsupported OIDC issuer")
		return nil, false
	}
}

// oidcResolveBySlug maps a signed slug claim to a tracked repo and
// cross-checks the request's own repo field against it. The signed claim is
// authoritative — the form field, if sent, only has to agree.
func (s *Server) oidcResolveBySlug(w http.ResponseWriter, r *http.Request, slug, forgeName string) (*store.Repo, bool) {
	if slug == "" {
		httpError(w, http.StatusForbidden, "oidc_invalid_token: the OIDC token has no repository claim")
		return nil, false
	}
	if formRepo := r.FormValue("repo"); formRepo != "" && formRepo != slug {
		httpError(w, http.StatusForbidden, "oidc_repo_mismatch: the OIDC token is for %q, not %q", slug, formRepo)
		return nil, false
	}
	repo, ws, ok := s.oidcLookup(w, r, slug, forgeName)
	if !ok {
		return nil, false
	}
	if repo == nil {
		// The slug is signed by the forge, so nothing more has to be
		// checked before registering it under its workspace.
		if repo, ok = s.oidcRegisterRepo(w, r, ws, slug); !ok {
			return nil, false
		}
	}
	s.log.Info("oidc upload verified", "repo", repo.Slug, "forge", forgeName)
	return repo, true
}

// oidcResolveBitbucket maps a Bitbucket OIDC token to a tracked repo. The
// token names the repo only by an opaque UUID (in repositoryUuid, and in
// sub as "{repo-uuid}:{step-uuid}"), so the request's repo slug is the
// lookup key and the tracked repo's forge-reported UUID is the check: the
// slug resolves a candidate repo, and its live UUID — fetched through that
// repo's own workspace connection, not the caller's — must equal the signed
// one. A slug pointing at another repo therefore fails on the UUID, so the
// untrusted slug can never redirect the upload.
func (s *Server) oidcResolveBitbucket(w http.ResponseWriter, r *http.Request, tok *oidc.Token) (*store.Repo, bool) {
	ctx := r.Context()
	wantUUID := bitbucketRepoUUID(tok)
	if wantUUID == "" {
		httpError(w, http.StatusForbidden, "oidc_invalid_token: the OIDC token has no repository id")
		return nil, false
	}
	slug := r.FormValue("repo")
	if slug == "" {
		httpError(w, http.StatusBadRequest, "Bitbucket OIDC uploads require the repo field")
		return nil, false
	}
	repo, ws, ok := s.oidcLookup(w, r, slug, "bitbucket")
	if !ok {
		return nil, false
	}

	// The UUID check below makes a live forge call through the repo's own
	// connection, so — like the fork-PR path (tokenless.go) — rate-limit it
	// per repo before making it: a valid token replayed with a victim's slug
	// must not be able to hammer that workspace's Bitbucket connection.
	if !s.tokenless.allow(slug, time.Now()) {
		httpError(w, http.StatusTooManyRequests, "OIDC upload rate limit reached for %s; try again later", slug)
		return nil, false
	}

	// Verify the slug→UUID binding through the repo's own workspace
	// connection: the caller does not choose which credentials answer this.
	// An untracked slug is verified through the workspace it would be
	// registered under — and registered only once the binding holds, so a
	// token replayed with a victim's slug cannot leave a repo row behind.
	var fg forge.Forge
	if repo != nil {
		var err error
		if fg, err = s.forges.For(ctx, repo); err != nil {
			s.internalError(w, "resolving forge connection", err)
			return nil, false
		}
	} else {
		fg = s.forges.Connected(ctx, ws, "bitbucket")
	}
	if fg == nil {
		httpError(w, http.StatusForbidden, "OIDC uploads for %s need its workspace connected to Bitbucket; a workspace owner can connect it from the workspace settings", ownerOf(slug))
		return nil, false
	}
	gotUUID, err := fg.GetRepoID(ctx, slug)
	if err != nil {
		s.log.Error("oidc bitbucket repo id", "repo", slug, "err", err)
		httpError(w, http.StatusBadGateway, "could not verify the repository identity with Bitbucket; retrying may help")
		return nil, false
	}
	if normalizeUUID(gotUUID) != normalizeUUID(wantUUID) {
		httpError(w, http.StatusForbidden, "oidc_repo_mismatch: the OIDC token's repository does not match %q", slug)
		return nil, false
	}
	if repo == nil {
		if repo, ok = s.oidcRegisterRepo(w, r, ws, slug); !ok {
			return nil, false
		}
	}
	s.log.Info("oidc upload verified", "repo", repo.Slug, "forge", "bitbucket")
	return repo, true
}

// oidcLookup resolves a slug to what an OIDC upload may target: the repo
// when it is tracked on the token's forge (ws is then nil — the caller
// has no use for it), or the registered workspace the slug falls under
// when it is not (repo is then nil, and the caller registers it once its
// own checks pass). A slug under no registered workspace is the one
// thing OIDC cannot create; that 404 is written here.
func (s *Server) oidcLookup(w http.ResponseWriter, r *http.Request, slug, forgeName string) (repo *store.Repo, ws *store.Workspace, ok bool) {
	ctx := r.Context()
	repo, err := s.store.RepoBySlug(ctx, forgeName, slug)
	switch {
	case err == nil:
		return repo, nil, true
	case !errors.Is(err, store.ErrNotFound):
		s.internalError(w, "looking up repo", err)
		return nil, nil, false
	}

	ws, err = s.forges.LookupWorkspace(ctx, slug, forgeName)
	if err != nil {
		s.internalError(w, "looking up workspace", err)
		return nil, nil, false
	}
	if ws == nil {
		httpError(w, http.StatusNotFound, "repo %q is not tracked on this server and its workspace is not registered here; "+
			"an OIDC upload registers the repo by itself once a workspace owner has registered %s", slug, ownerOf(slug))
		return nil, nil, false
	}
	return nil, ws, true
}

// oidcRegisterRepo registers a forge-verified slug under its workspace,
// with the same naming rules and forge existence check a workspace
// token's first upload goes through (resolveUploadRepo), writing the
// error response itself.
func (s *Server) oidcRegisterRepo(w http.ResponseWriter, r *http.Request, ws *store.Workspace, slug string) (*store.Repo, bool) {
	name := strings.TrimPrefix(slug, ws.Prefix+"/")
	if !core.ValidRepoName(ws.Forge, name) {
		httpError(w, http.StatusBadRequest, "invalid repo name %q under workspace %q", slug, ws.Prefix)
		return nil, false
	}
	repo, err := s.pipeline.RegisterRepo(r.Context(), ws, slug)
	if errors.Is(err, forge.ErrRepoNotFound) {
		httpError(w, http.StatusNotFound, "repo %q not found on %s", slug, ws.Forge)
		return nil, false
	}
	if err != nil {
		s.internalError(w, "auto-registering repo", err)
		return nil, false
	}
	s.log.Info("oidc upload registered repo", "repo", slug, "workspace", ws.Prefix)
	return repo, true
}

// bitbucketRepoUUID reads the repository UUID a Bitbucket token names its
// repo by: the repositoryUuid claim, falling back to the "{repo}:{step}"
// sub. Returned as the token carries it (braces and all); the caller
// normalizes before comparing.
func bitbucketRepoUUID(tok *oidc.Token) string {
	if u := tok.Claim("repositoryUuid"); u != "" {
		return u
	}
	if sub := tok.Subject; sub != "" {
		id, _, _ := strings.Cut(sub, ":")
		return id
	}
	return ""
}

// normalizeUUID folds the incidental differences between how Bitbucket's
// REST resource and its OIDC claims spell the same UUID — surrounding braces
// and letter case — so the comparison is on identity, not spelling.
func normalizeUUID(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), "{}"))
}
