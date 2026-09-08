// What a signed-in user may see. Access is by workspace membership: a
// user may view the repos under the prefixes of the workspaces they belong
// to, unless the instance is open (no sign-in configured), in which case
// everything is visible.

package server

import (
	"cmp"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/gocov/gocov/internal/core"
	"github.com/gocov/gocov/internal/store"
)

// userScope resolves the request user's workspace membership into a scope.
// Auth off → unscoped, so an open-mode instance behaves exactly as before
// M2. Auth on → the user's workspace prefixes; a missing user (which should
// not occur behind requireAuth) yields a deny-all scope rather than an open
// one.
func (s *Server) userScope(r *http.Request) (repoScope, error) {
	if !s.authEnabled() {
		return repoScope{scoped: false}, nil
	}
	prefixes := map[wsKey]bool{}
	if u := currentUser(r); u != nil {
		wss, err := s.store.ListWorkspacesForUser(r.Context(), u.ID)
		if err != nil {
			return repoScope{}, err
		}
		for _, ws := range wss {
			prefixes[wsKey{ws.Forge, ws.Prefix}] = true
		}
	}
	return repoScope{scoped: true, prefixes: prefixes}, nil
}

// wsKey names a workspace the way the store does: prefix on a forge. A
// membership in the GitHub org "acme" says nothing about the GitLab group
// "acme". An empty forge is the operator's GOCOV_ALLOWED_WORKSPACES
// entry, a plain name that applies on every forge.
type wsKey struct{ forge, prefix string }

// String is the key as the dashboard's ?ws= carries it, "forge/prefix";
// GitLab prefixes contain slashes of their own, which is fine because the
// forge is always the first segment. An operator entry is its bare name.
func (k wsKey) String() string {
	if k.forge == "" {
		return k.prefix
	}
	return k.forge + "/" + k.prefix
}

// owningWorkspace picks, from the candidates, the workspace owning the
// repo: on the repo's forge, with the most specific prefix (a project
// below a registered GitLab subgroup belongs to the subgroup, not to a
// same-named ancestor). Nil when none does.
func owningWorkspace(repo *store.Repo, candidates []*store.Workspace) *store.Workspace {
	for _, prefix := range core.SlugPrefixes(repo.Slug) { // longest first
		for _, ws := range candidates {
			if ws.Forge == repo.Forge && ws.Prefix == prefix {
				return ws
			}
		}
	}
	return nil
}

// repoScope captures which repos a request may see (M2/R3). When scoped is
// false the instance runs in open mode (D5) and every repo is visible;
// otherwise a repo is visible only when its workspace prefix, on its
// forge, is a member prefix.
type repoScope struct {
	scoped   bool
	prefixes map[wsKey]bool
}

// allows reports whether a repo falls within the scope. Any slash-boundary
// prefix of its slug may carry the membership — a GitLab workspace
// registered at subgroup depth covers the projects below it.
func (rs repoScope) allows(repo *store.Repo) bool {
	if !rs.scoped {
		return true
	}
	for _, prefix := range core.SlugPrefixes(repo.Slug) {
		if rs.prefixes[wsKey{repo.Forge, prefix}] {
			return true
		}
	}
	return false
}

// canView reports whether the request may see the given repo. Callers
// that fail the check 404 (D3: a non-member must not learn a repo exists).
func (s *Server) canView(r *http.Request, repo *store.Repo) (bool, error) {
	scope, err := s.userScope(r)
	if err != nil {
		return false, err
	}
	return scope.allows(repo), nil
}

// authorizeReport decides whether the request may see a repo's report
// pages — repo page, upload detail, source view, raw profile. Members (and
// open-mode instances) pass exactly as before. Anyone else passes only
// when the repo is effectively public: the forge reported it public, the
// repo's "Public reports" switch is on and the instance allows it
// (GOCOV_PUBLIC_REPORTS). A refused visitor gets reportNotFound's answer —
// the login redirect when signed out, the 404 page for a signed-in
// non-member of a non-public repo (D3) — so nothing about the repo leaks.
//
// ok is false when the refusal has been written. member reports whether
// the viewer passed by membership (or the instance being open) rather than
// through the public branch: the switch for member chrome like the
// settings button, which a signed-in stranger on a public repo must not
// see either.
func (s *Server) authorizeReport(w http.ResponseWriter, r *http.Request, repo *store.Repo) (member, ok bool) {
	allowed, err := s.canView(r, repo)
	if err != nil {
		s.internalError(w, "checking access", err)
		return false, false
	}
	if allowed {
		return true, true
	}
	if s.publicReports && repo.ReportsPublic() {
		// The cached "public" may have flipped on the forge; an aged
		// answer is re-verified in the background (mechanics in
		// core.ReverifyVisibilityIfStale) while this request still serves.
		s.pipeline.ReverifyVisibilityIfStale(repo)
		if currentUser(r) == nil {
			// The anonymous render is briefly cacheable — these pages are
			// the badge/SEO surface, so repeat crawler traffic should be
			// absorbed upstream. max-age stays short so turning the
			// "Public reports" switch off takes effect within a minute
			// even behind a shared cache, and Vary: Cookie keeps such a
			// cache from answering a signed-in member with the stored
			// anonymous variant.
			w.Header().Set("Cache-Control", "public, max-age=60")
			w.Header().Set("Vary", "Cookie")
		}
		return false, true
	}
	s.reportNotFound(w, r)
	return false, false
}

// reportUpload resolves the upload named by the {id} path value and its
// repo, then runs authorizeReport — the prelude of every upload-scoped
// report page (the upload page, the source view, the profile download).
// ok is false when the answer has been written: the not-found path for a
// malformed or unknown id, the refusal for a repo the viewer may not see.
func (s *Server) reportUpload(w http.ResponseWriter, r *http.Request) (*store.Upload, *store.Repo, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.reportNotFound(w, r)
		return nil, nil, false
	}
	upload, err := s.store.Upload(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.reportNotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.internalError(w, "loading upload", err)
		return nil, nil, false
	}
	repo, err := s.store.RepoByID(r.Context(), upload.RepoID)
	if err != nil {
		s.internalError(w, "loading repo for upload", err)
		return nil, nil, false
	}
	if _, ok := s.authorizeReport(w, r, repo); !ok {
		return nil, nil, false
	}
	return upload, repo, true
}

// reportNotFound answers a report-page lookup that found nothing. For an
// anonymous visitor the answer is the login redirect, exactly what a
// missing session got before public report pages existed — a 404 here
// would tell a signed-out browser which slugs and upload ids exist.
func (s *Server) reportNotFound(w http.ResponseWriter, r *http.Request) {
	if s.authEnabled() && currentUser(r) == nil {
		redirectToLogin(w, r)
		return
	}
	s.renderNotFound(w, r)
}

// publicView reports whether this render is the anonymous read-only view
// of a public repo's page — the only state the layout shows the sign-up
// band in. Report handlers call it after authorizeReport let the request
// through, so signed-out with auth enabled implies an effectively public
// repo.
func (s *Server) publicView(r *http.Request) bool {
	return s.authEnabled() && currentUser(r) == nil
}

// allowedWorkspaceSet is the D3 authorization rule: the operator's explicit
// GOCOV_ALLOWED_WORKSPACES list when set (plain names, forge ""), otherwise
// the workspaces this instance tracks — registered workspace prefixes plus
// the workspace part of every registered repo slug, each on its forge.
// Names are scoped per forge, so a tracked GitHub org "acme" admits nobody
// from the Bitbucket workspace "acme".
func (s *Server) allowedWorkspaceSet(r *http.Request) (map[wsKey]bool, error) {
	set := map[wsKey]bool{}
	if len(s.allowedWorkspaces) > 0 {
		for _, ws := range s.allowedWorkspaces {
			set[wsKey{"", ws}] = true
		}
		return set, nil
	}
	workspaces, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nil, err
	}
	for _, ws := range workspaces {
		set[wsKey{ws.Forge, ws.Prefix}] = true
	}
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		for _, prefix := range core.SlugPrefixes(repo.Slug) {
			set[wsKey{repo.Forge, prefix}] = true
		}
	}
	return set, nil
}

// admits reports whether the allow-set lets an account on the forge in
// through membership of the named workspace: tracked on that forge, or
// listed by the operator for every forge.
func admits(allowed map[wsKey]bool, forge, name string) bool {
	return allowed[wsKey{forge, name}] || allowed[wsKey{"", name}]
}

// trackedWorkspace is one entry of the login page's "tracked workspaces"
// line: the name, and the forge it is tracked on so a member of the
// same-named workspace elsewhere can see why they were turned away.
// Forge is the forge's label, empty for an operator-listed name.
type trackedWorkspace struct {
	Name  string
	Forge string
}

// trackedWorkspaces renders the allowed set for the login page, so it is
// obvious whose coverage an instance holds.
func (s *Server) trackedWorkspaces(r *http.Request) []trackedWorkspace {
	set, err := s.allowedWorkspaceSet(r)
	if err != nil {
		return nil
	}
	out := make([]trackedWorkspace, 0, len(set))
	for _, k := range sortedKeys(set) {
		tw := trackedWorkspace{Name: k.prefix}
		if k.forge != "" {
			tw.Forge = providerLabel(k.forge)
		}
		out = append(out, tw)
	}
	return out
}

// sortedKeys orders an allow-set by forge, then name — the store's own
// listing order.
func sortedKeys(set map[wsKey]bool) []wsKey {
	return slices.SortedFunc(maps.Keys(set), func(a, b wsKey) int {
		return cmp.Or(cmp.Compare(a.forge, b.forge), cmp.Compare(a.prefix, b.prefix))
	})
}
