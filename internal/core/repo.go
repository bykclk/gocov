// Registering a repo the first time an upload names it. A workspace token
// may upload for any repo under its prefix, so the repo row is created on
// demand — with the rules for what a repo may be called, since a slug from
// an upload is untrusted input.

package core

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/store"
)

// repoNameRe bounds the repo part of auto-registered slugs: one path
// segment, conservative charset, sane length.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// maxRepoNameSegments bounds a GitLab repo name's depth below the
// workspace; GitLab itself caps group nesting at 20 levels.
const maxRepoNameSegments = 21

// validRepoName validates the repo part of a workspace-token slug.
// GitLab projects can sit in subgroups below the registered namespace, so
// a gitlab name may span several path segments; other forges take exactly
// one.
func ValidRepoName(forgeName, name string) bool {
	segments := strings.Split(name, "/")
	if forgeName != "gitlab" && len(segments) != 1 {
		return false
	}
	if len(segments) > maxRepoNameSegments {
		return false
	}
	for _, s := range segments {
		// repoNameRe's charset admits "." and ".." — as path segments they
		// are traversal, not names.
		if s == "." || s == ".." || !repoNameRe.MatchString(s) {
			return false
		}
	}
	return true
}

// RegisterRepo registers a repo first seen through a workspace token. The
// workspace's connection is asked for the repo's default branch; having
// none, or one that cannot answer, is fine.
// The default branch is asked from the forge when the workspace has a
// one-click connection, then falls back to the workspace default and
// finally to "main". A forge that positively says the repo does not
// exist aborts the registration (ErrRepoNotFound), so a leaked workspace
// token cannot fill the dashboard with invented repos.
func (p *Pipeline) RegisterRepo(ctx context.Context, ws *store.Workspace, slug string) (*store.Repo, error) {
	branch := ""
	var fg forge.Forge
	if p.Forges != nil {
		fg = p.Forges.Connected(ctx, ws, ws.Forge)
	}
	if fg != nil {
		b, err := fg.GetDefaultBranch(ctx, slug)
		switch {
		case err == nil && b != "":
			branch = b
		case errors.Is(err, forge.ErrRepoNotFound):
			return nil, err
		case err != nil && !errors.Is(err, forge.ErrNotImplemented):
			// Transient forge trouble must not block a legitimate first
			// upload; fall back to the workspace default branch.
			p.Log.Warn("get default branch", "repo", slug, "err", err)
		}
	}
	branch = cmp.Or(branch, ws.DefaultBranch, "main")

	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	repo := &store.Repo{
		Forge:         ws.Forge,
		Slug:          slug,
		Token:         token,
		DefaultBranch: branch,
		Gate:          ws.Gate,
	}
	if err := p.Store.CreateRepo(ctx, repo); err != nil {
		// A concurrent first upload may have won the race; use its repo.
		if existing, lookupErr := p.Store.RepoBySlug(ctx, ws.Forge, slug); lookupErr == nil {
			return existing, nil
		}
		return nil, err
	}
	p.Log.Info("auto-registered repo", "slug", slug, "default_branch", branch, "workspace", ws.Prefix)
	return repo, nil
}

// How stale the cached forge visibility answer may grow on each path
// before it is re-asked. The Pipeline's VisibilityUploadTTL field
// overrides the upload TTL for tests.
const (
	// defaultVisibilityUploadTTL bounds the upload path: within it an
	// upload skips the visibility round-trip entirely, so a commit
	// uploading ten parts asks the forge once, not ten times.
	defaultVisibilityUploadTTL = time.Hour
	// defaultVisibilityServeTTL bounds the anonymous serving path: a
	// public report page served on an older answer triggers a background
	// re-verification, so a repo flipped private on the forge closes its
	// pages within the TTL even when its CI never uploads again.
	defaultVisibilityServeTTL = 24 * time.Hour
	// visibilityRecheckGap rate-limits background re-verification
	// attempts per repo — a hot public page must not hammer the forge
	// while an answer refuses to land (failures never stamp the row).
	visibilityRecheckGap = time.Minute
	// visibilityRecheckTimeout bounds the detached background check.
	visibilityRecheckTimeout = 15 * time.Second
)

func (p *Pipeline) uploadVisibilityTTL() time.Duration {
	return cmp.Or(p.VisibilityUploadTTL, defaultVisibilityUploadTTL)
}

// visibilityFresh reports whether the repo's cached visibility answer is
// younger than ttl. A zero stamp — the forge has never answered — is
// always stale.
func visibilityFresh(repo *store.Repo, ttl time.Duration) bool {
	return !repo.VisibilityCheckedAt.IsZero() && time.Since(repo.VisibilityCheckedAt) < ttl
}

// RefreshVisibility re-asks the forge whether the repo is public and
// caches the answer on the repo row — the switch behind anonymous report
// pages. Best effort: a transient failure keeps the last known state
// (private until the forge has ever answered) and never disturbs the
// caller; only a definitive answer — public, private, or the forge
// positively saying the repo is gone — changes anything. Callers decide
// when to ask (visibilityFresh); this always does.
func (p *Pipeline) RefreshVisibility(ctx context.Context, fg forge.Forge, repo *store.Repo) {
	if fg == nil {
		return
	}
	// The stamp is the time the question was asked, not answered or
	// written: of two racing answers (an in-flight refresh vs a
	// webhook-delivered flip, say) SetRepoVisibility then keeps whichever
	// reflects the later ask, regardless of write order.
	askedAt := time.Now()
	fv, err := fg.GetRepoVisibility(ctx, repo.Slug)
	switch {
	case errors.Is(err, forge.ErrNotImplemented):
		return
	case errors.Is(err, forge.ErrRepoNotFound):
		// Definitive: this connection can no longer see the repo —
		// deleted, or hidden from an installation that lost access.
		// Not-visible is certainly not public, so fail closed instead of
		// serving the old answer forever — loudly, because for a repo
		// merely outside an App's selected-repositories set this closes
		// legitimately public pages and the operator should see why.
		p.Log.Warn("repo not visible to its forge connection; caching private", "repo", repo.Slug, "err", err)
		fv = forge.VisibilityPrivate
	case err != nil:
		p.Log.Warn("get repo visibility", "repo", repo.Slug, "err", err)
		return
	}
	// Map the forge vocabulary onto the store's explicitly, so the two
	// constant sets are tied here rather than by incidental string
	// equality — and a forge answer outside the contract is rejected
	// instead of cached as an unrecognized (and thus private) value.
	var v string
	switch fv {
	case forge.VisibilityPublic:
		v = store.VisibilityPublic
	case forge.VisibilityPrivate:
		v = store.VisibilityPrivate
	default:
		p.Log.Warn("unknown repo visibility from forge", "repo", repo.Slug, "visibility", fv)
		return
	}
	// An unchanged answer is still persisted: SetRepoVisibility stamps
	// VisibilityCheckedAt, and both freshness windows count from the last
	// answer, not the last change.
	if err := p.Store.SetRepoVisibility(ctx, repo.ID, v, askedAt); err != nil {
		p.Log.Error("caching repo visibility", "repo", repo.Slug, "err", err)
		return
	}
	if v != repo.Visibility {
		p.Log.Info("repo visibility changed", "repo", repo.Slug, "visibility", v)
	}
	repo.Visibility = v
	repo.VisibilityCheckedAt = askedAt
}

// refreshVisibilityOnce is RefreshVisibility behind the per-repo
// in-flight guard: when another request is already asking the forge the
// same question — a commit's parts uploading concurrently, say — the
// duplicate is skipped and that request's answer serves everyone.
func (p *Pipeline) refreshVisibilityOnce(ctx context.Context, fg forge.Forge, repo *store.Repo) {
	if !p.beginVisibilityRefresh(repo.ID) {
		return
	}
	defer p.endVisibilityRefresh(repo.ID)
	p.RefreshVisibility(ctx, fg, repo)
}

func (p *Pipeline) beginVisibilityRefresh(repoID int64) bool {
	p.visMu.Lock()
	defer p.visMu.Unlock()
	if p.visInFlight[repoID] {
		return false
	}
	if p.visInFlight == nil {
		p.visInFlight = map[int64]bool{}
	}
	p.visInFlight[repoID] = true
	return true
}

func (p *Pipeline) endVisibilityRefresh(repoID int64) {
	p.visMu.Lock()
	defer p.visMu.Unlock()
	delete(p.visInFlight, repoID)
}

// ReverifyVisibility re-asks the repo's visibility through its
// workspace's forge connection right now — the shared re-check behind
// the serve-path staleness TTL and webhook-announced flips that must be
// verified before they can open pages. Without a connection nothing
// changes: the cached answer keeps its age.
func (p *Pipeline) ReverifyVisibility(ctx context.Context, repo *store.Repo) {
	fg, err := p.forgeFor(ctx, repo)
	if err != nil || fg == nil {
		return
	}
	p.refreshVisibilityOnce(ctx, fg, repo)
}

// MarkRepoPrivate caches a private flip the forge announced out of band
// (a webhook delivery). Trusting the announcement without re-asking is
// safe in this direction only — closing pages on stale news costs a
// public repo's visitors a sign-in until the next refresh, while opening
// on stale news would leak a private repo — so the opposite flip goes
// through ReverifyVisibility instead.
func (p *Pipeline) MarkRepoPrivate(ctx context.Context, repo *store.Repo) {
	if err := p.Store.SetRepoVisibility(ctx, repo.ID, store.VisibilityPrivate, time.Now()); err != nil {
		p.Log.Error("caching repo visibility", "repo", repo.Slug, "err", err)
		return
	}
	if repo.Visibility != store.VisibilityPrivate {
		p.Log.Info("repo visibility changed", "repo", repo.Slug, "visibility", store.VisibilityPrivate, "source", "webhook")
	}
	repo.Visibility = store.VisibilityPrivate
}

// ReverifyVisibilityIfStale starts a background re-check of a repo about
// to be served publicly when its cached forge answer has aged past the
// serve TTL. The current request still serves the cached answer — the
// check must not add a forge round-trip to a page load — but a repo
// flipped private on the forge closes for the requests after it, upload
// or no upload. Attempts are rate-limited per repo, and only a definitive
// forge answer changes anything (RefreshVisibility keeps the last known
// state on transient errors). Reports whether a re-check was started.
func (p *Pipeline) ReverifyVisibilityIfStale(repo *store.Repo) bool {
	if p.Forges == nil || !p.Forges.Capable(repo.Forge) ||
		visibilityFresh(repo, defaultVisibilityServeTTL) {
		// The Capable check keeps a forge this deployment has no
		// connector for from scheduling work whose outcome — no client,
		// keep the last known state — is already decided.
		return false
	}
	if !p.claimVisibilityRecheck(repo.ID) {
		return false
	}
	// Detached copy and context: the check outlives the request, and the
	// request's repo struct must not be written from another goroutine.
	r := *repo
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), visibilityRecheckTimeout)
		defer cancel()
		p.ReverifyVisibility(ctx, &r)
	}()
	return true
}

// claimVisibilityRecheck records a re-check attempt for the repo unless
// one was recorded within the rate-limit gap — the winner of concurrent
// page loads is the one goroutine that asks the forge.
func (p *Pipeline) claimVisibilityRecheck(repoID int64) bool {
	p.visMu.Lock()
	defer p.visMu.Unlock()
	now := time.Now()
	// An entry past the gap can no longer refuse anything — sweep them so
	// the map stays bounded by the repos re-checked in the last gap, not
	// by every repo (deleted ones included) ever served on a stale answer.
	maps.DeleteFunc(p.visChecks, func(_ int64, at time.Time) bool {
		return now.Sub(at) >= visibilityRecheckGap
	})
	if _, ok := p.visChecks[repoID]; ok {
		return false
	}
	if p.visChecks == nil {
		p.visChecks = map[int64]time.Time{}
	}
	p.visChecks[repoID] = now
	return true
}

// NewToken generates a token: 24 random bytes in hex. Repo and workspace
// tokens are the same shape, and only their hash is ever compared, so one
// generator serves both.
func NewToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SlugPrefixes returns every slash-boundary prefix of a repo slug,
// longest first: "a/b/c" → ["a/b", "a"]. GitLab namespaces nest, so a
// repo's workspace can sit at any depth (a registered subgroup path is a
// workspace of its own); Bitbucket and GitHub slugs only ever have the
// single-segment prefix.
func SlugPrefixes(slug string) []string {
	var out []string
	for {
		prefix, _, ok := strings.CutLast(slug, "/")
		if !ok {
			return out
		}
		out = append(out, prefix)
		slug = prefix
	}
}
