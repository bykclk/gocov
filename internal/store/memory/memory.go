// Package memory provides an in-memory store.Store for tests.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/gocov/gocov/internal/store"
)

// Store is an in-memory implementation of store.Store. Safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	repoSeq    int64
	upSeq      int64
	wsSeq      int64
	userSeq    int64
	crSeq      int64
	repos      map[int64]*store.Repo
	uploads    map[int64]*store.Upload
	files      map[int64][]*store.UploadFile // keyed by upload ID
	reports    map[int64]*store.CommitReport // merged reports, keyed by report ID
	crLocks    map[string]*sync.Mutex        // per-commit recompute locks
	grantLocks map[int64]*sync.Mutex         // per-workspace grant-refresh locks
	crPush     map[string]*sync.Mutex        // per-commit status-push locks
	crStatus   map[string]int64              // last pushed status version, keyed repoID:sha
	workspaces map[int64]*store.Workspace
	users      map[int64]*store.User
	sessions   map[string]*store.Session      // keyed by token hash
	members    map[int64]map[int64]store.Role // userID -> workspace ID -> role
	tokenless  map[string]bool                // accepted tokenless (repo, run, attempt, part) triples
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		repos:      map[int64]*store.Repo{},
		uploads:    map[int64]*store.Upload{},
		files:      map[int64][]*store.UploadFile{},
		reports:    map[int64]*store.CommitReport{},
		crLocks:    map[string]*sync.Mutex{},
		grantLocks: map[int64]*sync.Mutex{},
		crPush:     map[string]*sync.Mutex{},
		crStatus:   map[string]int64{},
		workspaces: map[int64]*store.Workspace{},
		users:      map[int64]*store.User{},
		sessions:   map[string]*store.Session{},
		members:    map[int64]map[int64]store.Role{},
		tokenless:  map[string]bool{},
	}
}

// find returns a value of m that keep admits, or nil. Every lookup here is
// by a column Postgres keeps unique, so at most one value can match and
// the map's iteration order does not matter. Callers hold s.mu.
func find[K comparable, V any](m map[K]*V, keep func(*V) bool) *V {
	for _, v := range m {
		if keep(v) {
			return v
		}
	}
	return nil
}

// atMost applies the limit convention of every list query: a positive limit
// caps the result and anything else means all of it.
func atMost[S ~[]E, E any](s S, limit int) S {
	if limit > 0 {
		return s[:min(len(s), limit)]
	}
	return s
}

// mutexFor hands out the mutex under key, creating it on first use so a
// commit or workspace that has never been locked needs no registration.
func (s *Store) mutexFor[K comparable](locks map[K]*sync.Mutex, key K) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := locks[key]
	if m == nil {
		m = new(sync.Mutex)
		locks[key] = m
	}
	return m
}

func (s *Store) CreateRepo(_ context.Context, r *store.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Mirror the Postgres UNIQUE constraints; autoCreateRepo's concurrent
	// registration fallback relies on duplicate slugs failing.
	if find(s.repos, func(x *store.Repo) bool {
		return (x.Forge == r.Forge && x.Slug == r.Slug) || x.Token == r.Token
	}) != nil {
		return fmt.Errorf("memory: repo slug or token already exists")
	}
	s.repoSeq++
	r.ID = s.repoSeq
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	cp := copyRepo(r)
	// Only SetRepoVisibility writes the stamp (see the Store contract); a
	// fresh row starts as "never asked", mirroring Postgres's NULL default.
	cp.VisibilityCheckedAt = time.Time{}
	s.repos[r.ID] = cp
	return nil
}

// copyRepo deep-copies a repo so callers and the store never alias: the
// slice field would otherwise be shared, and Postgres hands back a fresh
// one on every read.
func copyRepo(r *store.Repo) *store.Repo {
	cp := new(*r)
	cp.IgnorePaths = slices.Clone(r.IgnorePaths)
	return cp
}

func (s *Store) UpdateRepo(_ context.Context, r *store.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.repos[r.ID]
	if !ok {
		return store.ErrNotFound
	}
	cp := copyRepo(r)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = existing.CreatedAt
	}
	// Visibility and its checked-at stamp are SetRepoVisibility's alone
	// (see the Store contract): a full-row save must not revert a
	// concurrent refresh.
	cp.Visibility = existing.Visibility
	cp.VisibilityCheckedAt = existing.VisibilityCheckedAt
	s.repos[r.ID] = cp
	return nil
}

// The listing orders, matching postgres's ORDER BY forge, slug|prefix.
func byForgeSlug(a, b *store.Repo) int {
	return cmp.Or(cmp.Compare(a.Forge, b.Forge), cmp.Compare(a.Slug, b.Slug))
}

func byForgePrefix(a, b *store.Workspace) int {
	return cmp.Or(cmp.Compare(a.Forge, b.Forge), cmp.Compare(a.Prefix, b.Prefix))
}

func (s *Store) PublicRepoRefs(_ context.Context, limit int) ([]store.RepoRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var public []*store.Repo
	for _, r := range s.repos {
		if r.ReportsPublic() {
			public = append(public, r)
		}
	}
	slices.SortFunc(public, byForgeSlug)
	out := make([]store.RepoRef, 0, len(public))
	for _, r := range public {
		out = append(out, store.RepoRef{Forge: r.Forge, Slug: r.Slug})
	}
	return atMost(out, limit), nil
}

func (s *Store) SetRepoVisibility(_ context.Context, repoID int64, visibility string, checkedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoID]
	if !ok {
		return store.ErrNotFound
	}
	// A fresher answer already landed: skip, per the Store contract.
	if !r.VisibilityCheckedAt.IsZero() && !r.VisibilityCheckedAt.Before(checkedAt) {
		return nil
	}
	r.Visibility = visibility
	r.VisibilityCheckedAt = checkedAt
	return nil
}

func (s *Store) DeleteRepo(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.repos, id)
	s.deleteUploadsLocked(id)
	return nil
}

// deleteUploadsLocked drops a repo's uploads with their files, the
// uploads(repo_id) ON DELETE CASCADE chain. Callers hold s.mu.
func (s *Store) deleteUploadsLocked(repoID int64) {
	for uid, u := range s.uploads {
		if u.RepoID == repoID {
			delete(s.uploads, uid)
			delete(s.files, uid)
		}
	}
}

func (s *Store) RepoByID(_ context.Context, id int64) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyRepo(r), nil
}

func (s *Store) RepoBySlug(_ context.Context, forge, slug string) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := find(s.repos, func(r *store.Repo) bool { return r.Forge == forge && r.Slug == slug })
	if r == nil {
		return nil, store.ErrNotFound
	}
	return copyRepo(r), nil
}

func (s *Store) RepoByToken(_ context.Context, token string) (*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := find(s.repos, func(r *store.Repo) bool { return r.Token == token })
	if r == nil {
		return nil, store.ErrNotFound
	}
	return copyRepo(r), nil
}

func (s *Store) ListRepos(_ context.Context) ([]*store.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, copyRepo(r))
	}
	slices.SortFunc(out, byForgeSlug)
	return out, nil
}

func (s *Store) CreateWorkspace(_ context.Context, w *store.Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createWorkspaceLocked(w)
}

func (s *Store) createWorkspaceLocked(w *store.Workspace) error {
	if find(s.workspaces, func(x *store.Workspace) bool {
		return (x.Forge == w.Forge && x.Prefix == w.Prefix) || x.Token == w.Token
	}) != nil {
		return fmt.Errorf("memory: workspace prefix or token already exists")
	}
	s.wsSeq++
	w.ID = s.wsSeq
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now()
	}
	s.workspaces[w.ID] = new(*w)
	return nil
}

func (s *Store) RegisterWorkspace(_ context.Context, w *store.Workspace, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("memory: user %d does not exist", userID)
	}
	if err := s.createWorkspaceLocked(w); err != nil {
		return err
	}
	if s.members[userID] == nil {
		s.members[userID] = map[int64]store.Role{}
	}
	s.members[userID][w.ID] = store.RoleOwner
	return nil
}

func (s *Store) UpdateWorkspace(_ context.Context, w *store.Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.workspaces[w.ID]
	if !ok {
		return store.ErrNotFound
	}
	cp := new(*w)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = existing.CreatedAt
	}
	// Mirror postgres: the grant columns belong exclusively to their
	// SetWorkspace*Grant methods — the tokens rotate on every use, and a
	// full-row write from an earlier read would resurrect a dead one.
	cp.BitbucketGrantAccount = existing.BitbucketGrantAccount
	cp.BitbucketRefreshToken = existing.BitbucketRefreshToken
	cp.BitbucketGrantBroken = existing.BitbucketGrantBroken
	cp.GitLabGrantAccount = existing.GitLabGrantAccount
	cp.GitLabRefreshToken = existing.GitLabRefreshToken
	cp.GitLabGrantBroken = existing.GitLabGrantBroken
	s.workspaces[w.ID] = cp
	return nil
}

func (s *Store) SetWorkspaceBitbucketGrant(_ context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return s.setWorkspaceGrant(workspaceID, func(w *store.Workspace) {
		w.BitbucketGrantAccount, w.BitbucketRefreshToken, w.BitbucketGrantBroken = account, refreshToken, broken
	})
}

func (s *Store) SetWorkspaceGitLabGrant(_ context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return s.setWorkspaceGrant(workspaceID, func(w *store.Workspace) {
		w.GitLabGrantAccount, w.GitLabRefreshToken, w.GitLabGrantBroken = account, refreshToken, broken
	})
}

// setWorkspaceGrant applies set to the stored workspace under the lock.
func (s *Store) setWorkspaceGrant(workspaceID int64, set func(*store.Workspace)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[workspaceID]
	if !ok {
		return store.ErrNotFound
	}
	set(w)
	return nil
}

func (s *Store) DeleteWorkspace(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws, ok := s.workspaces[id]
	if !ok {
		return store.ErrNotFound
	}
	// Cascade repos under the prefix along with their uploads, upload
	// files and reports — mirroring the postgres ON DELETE CASCADE chain.
	for rid, r := range s.repos {
		if !ws.Owns(r) {
			continue
		}
		s.deleteUploadsLocked(rid)
		maps.DeleteFunc(s.reports, func(_ int64, cr *store.CommitReport) bool { return cr.RepoID == rid })
		delete(s.repos, rid)
	}
	delete(s.workspaces, id)
	for _, m := range s.members {
		delete(m, id)
	}
	return nil
}

func (s *Store) WorkspaceByPrefix(_ context.Context, forge, prefix string) (*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := find(s.workspaces, func(w *store.Workspace) bool { return w.Forge == forge && w.Prefix == prefix })
	if w == nil {
		return nil, store.ErrNotFound
	}
	return new(*w), nil
}

func (s *Store) WorkspaceByToken(_ context.Context, token string) (*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := find(s.workspaces, func(w *store.Workspace) bool { return w.Token == token })
	if w == nil {
		return nil, store.ErrNotFound
	}
	return new(*w), nil
}

func (s *Store) ListWorkspaces(_ context.Context) ([]*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Workspace, 0, len(s.workspaces))
	for _, w := range s.workspaces {
		out = append(out, new(*w))
	}
	slices.SortFunc(out, byForgePrefix)
	return out, nil
}

func (s *Store) SetUserMemberships(_ context.Context, userID int64, memberships []store.Membership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(memberships) == 0 {
		delete(s.members, userID)
		return nil
	}
	set := make(map[int64]store.Role, len(memberships))
	for _, m := range memberships {
		set[m.WorkspaceID] = m.Role
	}
	s.members[userID] = set
	return nil
}

func (s *Store) ListMembershipsForUser(_ context.Context, userID int64) ([]store.Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Membership, 0, len(s.members[userID]))
	for wsID, role := range s.members[userID] {
		if _, ok := s.workspaces[wsID]; ok {
			out = append(out, store.Membership{WorkspaceID: wsID, Role: role})
		}
	}
	slices.SortFunc(out, func(a, b store.Membership) int { return cmp.Compare(a.WorkspaceID, b.WorkspaceID) })
	return out, nil
}

func (s *Store) ListWorkspacesForUser(_ context.Context, userID int64) ([]*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Workspace, 0, len(s.members[userID]))
	for wsID := range s.members[userID] {
		if w, ok := s.workspaces[wsID]; ok {
			out = append(out, new(*w))
		}
	}
	slices.SortFunc(out, byForgePrefix)
	return out, nil
}

func (s *Store) UpsertUser(_ context.Context, u *store.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if existing := find(s.users, func(x *store.User) bool { return x.Forge == u.Forge && x.ForgeUUID == u.ForgeUUID }); existing != nil {
		existing.Email = u.Email
		existing.DisplayName = u.DisplayName
		existing.ForgeWorkspaces = slices.Clone(u.ForgeWorkspaces)
		existing.ForgeOwnedWorkspaces = slices.Clone(u.ForgeOwnedWorkspaces)
		existing.LastLoginAt = now
		*u = *copyUser(existing)
		return nil
	}
	s.userSeq++
	u.ID = s.userSeq
	u.CreatedAt = now
	u.LastLoginAt = now
	s.users[u.ID] = copyUser(u)
	return nil
}

// copyUser deep-copies a user so callers and the store never alias the
// forge-workspace snapshots, matching the postgres JSON round-trip.
func copyUser(u *store.User) *store.User {
	cp := new(*u)
	cp.ForgeWorkspaces = slices.Clone(u.ForgeWorkspaces)
	cp.ForgeOwnedWorkspaces = slices.Clone(u.ForgeOwnedWorkspaces)
	return cp
}

func (s *Store) UserByID(_ context.Context, id int64) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyUser(u), nil
}

func (s *Store) ListUsers(_ context.Context) ([]*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, copyUser(u))
	}
	slices.SortFunc(out, func(a, b *store.User) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *Store) DeleteUser(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.users, id)
	delete(s.members, id)
	maps.DeleteFunc(s.sessions, func(_ string, sess *store.Session) bool { return sess.UserID == id })
	return nil
}

func (s *Store) CreateSession(_ context.Context, sess *store.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[sess.UserID]; !ok {
		return fmt.Errorf("memory: session user %d does not exist", sess.UserID)
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	s.sessions[sess.TokenHash] = new(*sess)
	return nil
}

func (s *Store) UserBySession(_ context.Context, tokenHash string) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tokenHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	if !sess.ExpiresAt.After(time.Now()) {
		delete(s.sessions, tokenHash) // lazy cleanup, matching postgres
		return nil, store.ErrNotFound
	}
	u, ok := s.users[sess.UserID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyUser(u), nil
}

func (s *Store) DeleteSession(_ context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[tokenHash]; !ok {
		return store.ErrNotFound
	}
	delete(s.sessions, tokenHash)
	return nil
}

func (s *Store) CreateUpload(_ context.Context, u *store.Upload, files []*store.UploadFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upSeq++
	u.ID = s.upSeq
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	s.uploads[u.ID] = copyUpload(u)
	fs := make([]*store.UploadFile, 0, len(files))
	for _, f := range files {
		fcp := new(*f)
		fcp.UploadID = u.ID
		fs = append(fs, fcp)
	}
	slices.SortFunc(fs, func(a, b *store.UploadFile) int { return cmp.Compare(a.Path, b.Path) })
	s.files[u.ID] = fs
	return nil
}

func (s *Store) Upload(_ context.Context, id int64) (*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyUpload(u), nil
}

// copyUpload deep-copies an upload so callers and the store never alias the
// same DiffCoverage, matching the postgres JSON round-trip semantics.
func copyUpload(u *store.Upload) *store.Upload {
	cp := new(*u)
	cp.DiffCoverage = u.DiffCoverage.Clone()
	return cp
}

func (s *Store) ListUploads(_ context.Context, repoID int64, limit int) ([]*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newestUploads(limit, func(u *store.Upload) bool { return u.RepoID == repoID }), nil
}

func (s *Store) ListBranchUploads(_ context.Context, repoID int64, branch string, limit int) ([]*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newestUploads(limit, func(u *store.Upload) bool { return u.RepoID == repoID && u.Branch == branch }), nil
}

// newestUploads returns copies of the uploads keep admits, newest first and
// capped at limit — the shape of every upload listing, since Postgres pages
// them by descending ID. Callers hold s.mu.
func (s *Store) newestUploads(limit int, keep func(*store.Upload) bool) []*store.Upload {
	var out []*store.Upload
	for _, u := range s.uploads {
		if keep(u) {
			out = append(out, copyUpload(u))
		}
	}
	slices.SortFunc(out, func(a, b *store.Upload) int { return cmp.Compare(b.ID, a.ID) })
	return atMost(out, limit)
}

func (s *Store) UploadFiles(_ context.Context, uploadID int64) ([]*store.UploadFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.files[uploadID]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := make([]*store.UploadFile, 0, len(fs))
	for _, f := range fs {
		out = append(out, new(*f))
	}
	return out, nil
}

func (s *Store) LatestUploadsPerPart(_ context.Context, repoID int64, commitSHA string) ([]*store.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[string]*store.Upload{}
	for _, u := range s.uploads {
		if u.RepoID != repoID || u.CommitSHA != commitSHA {
			continue
		}
		if cur, ok := latest[u.Part]; !ok || u.ID > cur.ID {
			latest[u.Part] = u
		}
	}
	out := make([]*store.Upload, 0, len(latest))
	for _, u := range latest {
		out = append(out, copyUpload(u))
	}
	slices.SortFunc(out, func(a, b *store.Upload) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *Store) WithCommitReportTx(ctx context.Context, repoID int64, commitSHA string, fn func(context.Context, store.CommitTx) error) error {
	m := s.mutexFor(s.crLocks, fmt.Sprintf("%d:%s", repoID, commitSHA))
	m.Lock()
	defer m.Unlock()
	// The store's own methods satisfy store.CommitTx; the per-commit mutex
	// gives the same serialization the Postgres advisory lock does.
	return fn(ctx, s)
}

// WithGrantLock serializes fn per workspace with a mutex — within one
// process that is exactly the guarantee the Postgres advisory lock gives
// across many. The store's own methods satisfy store.GrantTx.
func (s *Store) WithGrantLock(ctx context.Context, workspaceID int64, fn func(context.Context, store.GrantTx) error) error {
	m := s.mutexFor(s.grantLocks, workspaceID)
	m.Lock()
	defer m.Unlock()
	return fn(ctx, s)
}

func (s *Store) UpsertCommitReport(_ context.Context, cr *store.CommitReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if existing := s.commitReportLocked(cr.RepoID, cr.CommitSHA); existing != nil {
		// Preserve id and first-seen created_at across recomputes.
		cr.ID = existing.ID
		cr.CreatedAt = existing.CreatedAt
		cr.UpdatedAt = now
		s.reports[cr.ID] = copyCommitReport(cr)
		return nil
	}
	s.crSeq++
	cr.ID = s.crSeq
	if cr.CreatedAt.IsZero() {
		cr.CreatedAt = now
	}
	cr.UpdatedAt = now
	s.reports[cr.ID] = copyCommitReport(cr)
	return nil
}

func (s *Store) CommitReport(_ context.Context, repoID int64, commitSHA string) (*store.CommitReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cr := s.commitReportLocked(repoID, commitSHA)
	if cr == nil {
		return nil, store.ErrNotFound
	}
	return copyCommitReport(cr), nil
}

// commitReportLocked returns the stored report for a commit, or nil; there
// is at most one, as (repo_id, commit_sha) is unique. Callers hold s.mu.
func (s *Store) commitReportLocked(repoID int64, commitSHA string) *store.CommitReport {
	return find(s.reports, func(cr *store.CommitReport) bool { return cr.RepoID == repoID && cr.CommitSHA == commitSHA })
}

func (s *Store) LatestCommitReport(_ context.Context, repoID int64, branch string) (*store.CommitReport, error) {
	return s.latestCommitReport(repoID, branch, "", false, false)
}

func (s *Store) LatestNonPRCommitReport(_ context.Context, repoID int64, branch string) (*store.CommitReport, error) {
	return s.latestCommitReport(repoID, branch, "", false, true)
}

func (s *Store) LatestPassedCommitReport(_ context.Context, repoID int64, branch, excludeCommit string) (*store.CommitReport, error) {
	return s.latestCommitReport(repoID, branch, excludeCommit, true, true)
}

func (s *Store) latestCommitReport(repoID int64, branch, excludeCommit string, passedOnly, nonPROnly bool) (*store.CommitReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *store.CommitReport
	for _, cr := range s.reports {
		if cr.RepoID != repoID || cr.Branch != branch {
			continue
		}
		if passedOnly && cr.GateFailed {
			continue
		}
		if nonPROnly && cr.PRID != "" {
			continue
		}
		if excludeCommit != "" && cr.CommitSHA == excludeCommit {
			continue
		}
		if latest == nil || cr.ID > latest.ID {
			latest = cr
		}
	}
	if latest == nil {
		return nil, store.ErrNotFound
	}
	return copyCommitReport(latest), nil
}

func (s *Store) TryPushStatus(ctx context.Context, repoID int64, commitSHA string, version int64, push func(context.Context) error) (bool, error) {
	key := fmt.Sprintf("%d:%s", repoID, commitSHA)
	// Serialize the whole check-push-advance per commit, mirroring the
	// Postgres advisory lock.
	m := s.mutexFor(s.crPush, key)
	m.Lock()
	defer m.Unlock()

	s.mu.Lock()
	exists := s.commitReportLocked(repoID, commitSHA) != nil
	cur := s.crStatus[key]
	s.mu.Unlock()
	if !exists {
		return false, nil // no report to attach a status to
	}
	if version < cur {
		return false, nil // a newer push already owns the status
	}
	if err := push(ctx); err != nil {
		return false, err // version not advanced, so a retry can push
	}
	s.mu.Lock()
	s.crStatus[key] = version
	s.mu.Unlock()
	return true, nil
}

func (s *Store) CommitParts(_ context.Context, repoID int64, commitSHA string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, u := range s.uploads {
		if u.RepoID == repoID && u.CommitSHA == commitSHA && !seen[u.Part] {
			seen[u.Part] = true
			out = append(out, u.Part)
		}
	}
	return out, nil
}

func (s *Store) ListBranchCommitReports(_ context.Context, repoID int64, branch string, limit int) ([]*store.CommitReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.CommitReport
	for _, cr := range s.reports {
		if cr.RepoID == repoID && cr.Branch == branch {
			out = append(out, copyCommitReport(cr))
		}
	}
	slices.SortFunc(out, func(a, b *store.CommitReport) int { return cmp.Compare(b.ID, a.ID) })
	return atMost(out, limit), nil
}

func (s *Store) ClaimTokenlessUpload(_ context.Context, repoID, runID, runAttempt int64, part string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := tokenlessKey(repoID, runID, runAttempt, part)
	if s.tokenless[key] {
		return false, nil
	}
	s.tokenless[key] = true
	return true, nil
}

func (s *Store) ReleaseTokenlessUpload(_ context.Context, repoID, runID, runAttempt int64, part string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokenless, tokenlessKey(repoID, runID, runAttempt, part))
	return nil
}

func tokenlessKey(repoID, runID, runAttempt int64, part string) string {
	return fmt.Sprintf("%d:%d:%d:%s", repoID, runID, runAttempt, part)
}

// copyCommitReport deep-copies a report so callers never alias the stored
// DiffCoverage, matching the Postgres JSON round-trip.
func copyCommitReport(cr *store.CommitReport) *store.CommitReport {
	cp := new(*cr)
	cp.DiffCoverage = cr.DiffCoverage.Clone()
	return cp
}
