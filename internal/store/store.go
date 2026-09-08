// Package store defines the storage interface and its domain types.
// Implementations: postgres (production), memory (tests).
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gocov/gocov/internal/diffcov"
	"github.com/gocov/gocov/internal/profile"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: not found")

// Gate holds the optional coverage requirements enforced on uploads.
// A nil field means that rule is not applied.
type Gate struct {
	// MinCoverage is the minimum acceptable total coverage percentage.
	MinCoverage *float64
	// MinDiffCoverage is the minimum acceptable diff coverage percentage
	// for PR uploads with executable changed lines.
	MinDiffCoverage *float64
	// MaxCoverageDrop is how many percentage points total coverage may
	// fall relative to the comparison baseline; 0 forbids any drop.
	MaxCoverageDrop *float64
}

// Configured reports whether any gate rule is set.
func (g Gate) Configured() bool {
	return g.MinCoverage != nil || g.MinDiffCoverage != nil || g.MaxCoverageDrop != nil
}

// RepoRef names a repo the way its URLs do: forge, then slug.
type RepoRef struct {
	Forge string
	Slug  string
}

// Workspace groups repos under a slug prefix ("workspace" in
// "workspace/repo"). Its token authorizes uploads for every repo with
// that prefix; unknown repos are auto-registered on first upload.
type Workspace struct {
	ID     int64
	Forge  string // forge for auto-created repos, "bitbucket" for now
	Prefix string // e.g. "myworkspace"
	Token  string
	// DefaultBranch is assigned to auto-created repos when the forge
	// cannot be asked for the real one.
	DefaultBranch string
	// Gate is copied to auto-created repos at registration time.
	Gate Gate
	// ReportRetentionDays is how long coverage uploads are kept before
	// pruning; 0 keeps them forever. Recorded from the settings UI; the
	// job that prunes on it lands separately.
	ReportRetentionDays int
	// GitHubInstallationID links the workspace to a GitHub App
	// installation (One-Click Connect D3); 0 when not connected. When
	// set, installation tokens outrank every stored credential (D4).
	GitHubInstallationID int64
	// GitHubAppBroken marks a connection whose installation token was
	// refused — the app was uninstalled or suspended on GitHub. Set
	// lazily on the first failing mint, cleared on reconnect or when a
	// mint succeeds again; the settings page renders it as "reconnect".
	GitHubAppBroken bool
	// BitbucketGrantAccount is the username of the Bitbucket account
	// that granted the workspace connect (One-Click Connect D6); posts
	// visibly carry this identity (D8). Empty when not connected.
	BitbucketGrantAccount string
	// BitbucketRefreshToken is the grant's rotating refresh token,
	// encrypted at rest by the postgres store (AES-GCM under
	// GOCOV_SECRET_KEY). Empty when not connected — or when decryption
	// failed, in which case BitbucketGrantBroken is set on the loaded
	// struct so the UI asks for a reconnect instead of erroring.
	BitbucketRefreshToken string
	// BitbucketGrantBroken mirrors GitHubAppBroken for the Bitbucket
	// grant: set lazily when a refresh comes back invalid_grant.
	BitbucketGrantBroken bool
	// GitLabGrantAccount, GitLabRefreshToken and GitLabGrantBroken
	// mirror the Bitbucket grant fields for GitLab Connect: the granting
	// account's username (notes post as it), the rotating refresh token
	// (encrypted at rest; empty with the broken flag set when
	// undecryptable) and the lazily-set revocation flag.
	GitLabGrantAccount string
	GitLabRefreshToken string
	GitLabGrantBroken  bool
	CreatedAt          time.Time
}

// Repo visibility values, as the forge last reported them. The empty
// string means the forge has never been asked; everything but "public"
// is treated as private.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Repo is a tracked repository. Slug is namespaced ("workspace/repo").
type Repo struct {
	ID            int64
	Forge         string // "bitbucket" for now
	Slug          string
	Token         string // per-repo upload token
	DefaultBranch string
	// Gate holds the repo's coverage requirements; violations turn the
	// pushed build status into a failure.
	Gate Gate
	// Visibility caches what the forge last reported about the repo:
	// VisibilityPublic or VisibilityPrivate. Refreshed on upload and
	// re-verified in the background when report pages are served
	// anonymously on a stale answer; empty until the forge has been
	// asked, which counts as private.
	Visibility string
	// VisibilityCheckedAt is when the forge last answered the visibility
	// question (stamped by SetRepoVisibility); zero when it never has.
	// Both freshness windows — the upload path's skip and the serving
	// path's background re-verification — count from it.
	VisibilityCheckedAt time.Time
	// PublicReportsDisabled is the repo-settings "Public reports" switch,
	// inverted so the zero value keeps the default: a public repo's
	// report pages are anonymously viewable unless a member turns them off.
	PublicReportsDisabled bool
	// IgnorePaths are the repo's ignore patterns (internal/ignore): report
	// paths matching any of them are dropped from every upload before its
	// totals, diff coverage, gate and merge are computed. Validated on
	// save; the upload path may add its own on top.
	IgnorePaths []string
	CreatedAt   time.Time
}

// ReportsPublic reports whether the repo's report pages may be served to
// anonymous visitors: the forge says the repo is public and the repo's
// "Public reports" switch has not been turned off. The instance-level
// switch (GOCOV_PUBLIC_REPORTS) is the server's to apply on top.
func (r *Repo) ReportsPublic() bool {
	return r.Visibility == VisibilityPublic && !r.PublicReportsDisabled
}

// Upload is one coverage report for a commit.
type Upload struct {
	ID           int64
	RepoID       int64
	CommitSHA    string
	Branch       string
	PRID         string // empty when not a PR build
	Format       string
	TotalPct     float64
	CoveredStmts int64
	TotalStmts   int64
	RawBlobKey   string // blobstore key of the raw profile
	// DiffCoverage is set for PR uploads when the PR diff could be
	// fetched from the forge; nil otherwise.
	DiffCoverage *diffcov.Result
	// GateFailed marks uploads that violated the coverage gate; they are
	// excluded from comparison baselines.
	GateFailed bool
	// PathPrefix maps profile paths to repo-relative paths (e.g. the Go
	// module path), as sent with the upload.
	PathPrefix string
	// Part names the slice of the commit this upload covers (e.g. "backend",
	// "frontend"). Uploads with no explicit part carry "default". The merged
	// report reads the latest upload per (commit, part), so re-uploading a
	// part replaces it rather than accumulating.
	Part      string
	CreatedAt time.Time
	// Meta carries optional provenance captured at upload time — who
	// uploaded, from which CI run, how the profile arrived. Every field is
	// best-effort: uploads made before it was recorded, or through the raw
	// API, leave it empty.
	Meta UploadMeta
}

// UploadMeta is the optional provenance of an upload, stored as JSON. The
// CLI fills the source fields from the CI environment and git; the server
// fills the profile size, filename and its own processing time.
type UploadMeta struct {
	Uploader      string `json:"uploader,omitempty"`       // e.g. "gocov v0.9.2"
	UploaderKind  string `json:"uploader_kind,omitempty"`  // "cli" or "action"
	CIProvider    string `json:"ci_provider,omitempty"`    // "github", "gitlab", "bitbucket"
	CIRunURL      string `json:"ci_run_url,omitempty"`     // link to the CI run
	CommitMessage string `json:"commit_message,omitempty"` // first line of the commit message
	CommitAuthor  string `json:"commit_author,omitempty"`  // commit author display name
	ProfileName   string `json:"profile_name,omitempty"`   // original uploaded filename
	ProfileBytes  int64  `json:"profile_bytes,omitempty"`  // raw profile size
	ProcessMillis int64  `json:"process_millis,omitempty"` // server processing time
	// Tokenless marks an upload authenticated by workflow-run
	// verification instead of a bearer token (a fork PR's CI). Set by
	// the server only, never from a request field: the UI renders it as
	// "unverified contributor upload".
	Tokenless bool `json:"tokenless,omitzero"`
	// IgnoredFiles is how many report files the repo's and the upload's
	// ignore patterns dropped before the upload was measured; 0 when none
	// matched or no pattern was set.
	IgnoredFiles int `json:"ignored_files,omitzero"`
}

// User is a web UI account, identified by the forge account it signed in
// with. Users are provisioned just-in-time on first authorized login; no
// passwords are ever stored.
type User struct {
	ID    int64
	Forge string // "bitbucket" for now
	// ForgeUUID is the forge's stable account identifier (survives
	// renames). Unique per forge.
	ForgeUUID   string
	Email       string
	DisplayName string
	// ForgeWorkspaces are the workspace slugs the forge reported at the
	// last sign-in (M3/D3). OAuth tokens are discarded at login, so this
	// snapshot is what the registration page renders from; it goes stale
	// until the next login, never fresher.
	ForgeWorkspaces []string
	// ForgeOwnedWorkspaces is the subset of ForgeWorkspaces the account
	// administers on the forge, captured alongside it.
	ForgeOwnedWorkspaces []string
	CreatedAt            time.Time
	LastLoginAt          time.Time
}

// Owns reports whether the repo belongs to this workspace: same forge,
// slug below the prefix. Repos carry no workspace id; this is the tie
// (the M2 convention), and DeleteWorkspace cascades by the same rule.
func (w *Workspace) Owns(r *Repo) bool {
	return r.Forge == w.Forge && strings.HasPrefix(r.Slug, w.Prefix+"/")
}

// Role is what a membership lets its holder do in a workspace. It mirrors
// the forge's own role for the account and is re-synced at every sign-in.
type Role string

const (
	// RoleOwner administers the workspace: tokens, gates, forge
	// connection, deletion.
	RoleOwner Role = "owner"
	// RoleMember sees the workspace's reports and nothing more.
	RoleMember Role = "member"
)

// Membership is one user's seat in one workspace.
type Membership struct {
	WorkspaceID int64
	Role        Role
}

// Session is a server-side web UI session. Only a hash of the session
// token is stored, never the token itself.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CommitReport is the merged coverage of a commit, derived from the latest
// upload of each part and recomputed on every upload. It is the source of
// truth for status, gate, PR comment, insights, badge and trend, so a
// commit whose parts arrive in separate CI jobs reports their combined
// total rather than whichever part uploaded last. A commit with a single
// upload has a single-part report equal to that upload.
type CommitReport struct {
	ID           int64
	RepoID       int64
	CommitSHA    string
	Branch       string
	PRID         string // empty when not a PR build
	TotalPct     float64
	CoveredStmts int64
	TotalStmts   int64
	// GateFailed marks reports that violated the coverage gate; they are
	// excluded from comparison baselines, the same rule uploads carried.
	GateFailed bool
	// DiffCoverage is the merged diff coverage for PR commits; nil otherwise.
	DiffCoverage *diffcov.Result
	// PartCount is how many parts (distinct upload parts) fed the report.
	PartCount int
	// UploadID is the latest upload that fed this report; the trend links a
	// point to its upload detail page through it.
	UploadID  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UploadFile is per-file coverage within an upload. Blocks keep the full
// normalized block data so diff coverage can be computed later.
type UploadFile struct {
	UploadID     int64
	Path         string
	Pct          float64
	CoveredStmts int64
	TotalStmts   int64
	Blocks       []profile.Block
}

// Store is the persistence interface used by the server.
type Store interface {
	CreateRepo(ctx context.Context, r *Repo) error
	// UpdateRepo replaces the stored row matching r.ID with r's fields —
	// except Visibility and VisibilityCheckedAt, which only
	// SetRepoVisibility writes: the upload path refreshes them
	// concurrently with settings saves, and a full-row save carrying a
	// stale value must not revert them.
	UpdateRepo(ctx context.Context, r *Repo) error
	// DeleteRepo removes a repo together with its uploads and per-file rows.
	// Raw profile blobs are not touched; callers clean those up first.
	DeleteRepo(ctx context.Context, id int64) error
	// SetRepoVisibility updates only the repo's cached forge visibility —
	// a narrow UPDATE, because it is written from the upload path and must
	// not clobber (or be clobbered by) a concurrent settings save. checkedAt
	// says when the answer was obtained and becomes VisibilityCheckedAt; a
	// write whose checkedAt is not newer than the stored stamp is skipped
	// silently (nil), so an answer from an in-flight forge ask cannot
	// overwrite a fresher one — e.g. a webhook-delivered flip.
	SetRepoVisibility(ctx context.Context, repoID int64, visibility string, checkedAt time.Time) error
	RepoByID(ctx context.Context, id int64) (*Repo, error)
	// RepoBySlug resolves a repo by its forge and slug. Slugs are unique
	// per forge, not globally: github.com/acme/api and gitlab.com/acme/api
	// are two repos.
	RepoBySlug(ctx context.Context, forge, slug string) (*Repo, error)
	RepoByToken(ctx context.Context, token string) (*Repo, error)
	ListRepos(ctx context.Context) ([]*Repo, error)
	// PublicRepoRefs returns the forge and slug of repos whose report
	// pages are effectively public (forge-reported public, "Public
	// reports" switch on), ordered by forge then slug and capped at
	// limit; limit <= 0 means all. Feeds the sitemap without hydrating
	// full rows on a crawler-facing endpoint.
	PublicRepoRefs(ctx context.Context, limit int) ([]RepoRef, error)

	CreateWorkspace(ctx context.Context, w *Workspace) error
	// UpdateWorkspace replaces the stored row matching w.ID with w's fields.
	UpdateWorkspace(ctx context.Context, w *Workspace) error
	// DeleteWorkspace removes the workspace and cascades: every repo on the
	// same forge whose slug sits under the workspace prefix is deleted too, taking its
	// uploads and coverage reports with it. Memberships cascade away with
	// the workspace row. Nothing is touched on the forge.
	DeleteWorkspace(ctx context.Context, id int64) error
	// WorkspaceByPrefix resolves a workspace by its forge and prefix.
	// Prefixes are unique per forge, not globally: the GitHub org "acme"
	// and the GitLab group "acme" are two tenants.
	WorkspaceByPrefix(ctx context.Context, forge, prefix string) (*Workspace, error)
	WorkspaceByToken(ctx context.Context, token string) (*Workspace, error)
	ListWorkspaces(ctx context.Context) ([]*Workspace, error)
	// RegisterWorkspace creates the workspace and makes userID its first
	// member — an owner — atomically: self-service registration (M3) must
	// never leave a workspace nobody can see or administer.
	RegisterWorkspace(ctx context.Context, w *Workspace, userID int64) error
	// SetWorkspaceBitbucketGrant updates only the Bitbucket grant fields.
	// Bitbucket rotates refresh tokens on every use, so the swap must be
	// a single narrow UPDATE that cannot clobber (or be clobbered by) a
	// concurrent full-row settings save.
	SetWorkspaceBitbucketGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
	// SetWorkspaceGitLabGrant is SetWorkspaceBitbucketGrant's GitLab
	// twin, with the same rotation-safety contract.
	SetWorkspaceGitLabGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
	// WithGrantLock serializes refreshes of one workspace's Bitbucket or
	// GitLab grant across every server instance sharing the store, not
	// just within one process. Both forges rotate the refresh token on
	// every use, so two instances refreshing the same grant at once would
	// each invalidate the token the other just stored and drop the grant
	// — the one thing a rolling deploy, which briefly runs two instances,
	// must not do. fn re-reads the stored token through the passed
	// GrantTx (the freshest one, from whoever held the lock before it)
	// and persists the rotated one through it too; the lock is released
	// when fn returns. Locks on different workspaces never contend.
	WithGrantLock(ctx context.Context, workspaceID int64, fn func(ctx context.Context, tx GrantTx) error) error

	// SetUserMemberships replaces a user's workspace memberships with the
	// given set: memberships not listed are removed, listed ones are added
	// or have their role updated, so re-running it with the same set is a
	// no-op. Called at login to mirror the user's current forge membership
	// and role (M2).
	SetUserMemberships(ctx context.Context, userID int64, memberships []Membership) error
	// ListWorkspacesForUser returns the workspaces the user is a member of,
	// ordered by forge, then prefix.
	ListWorkspacesForUser(ctx context.Context, userID int64) ([]*Workspace, error)
	// ListMembershipsForUser returns the user's memberships with their
	// roles, ordered by workspace ID.
	ListMembershipsForUser(ctx context.Context, userID int64) ([]Membership, error)

	// UpsertUser creates the user on first login or, when a row with the
	// same forge+ForgeUUID exists, refreshes its email, display name,
	// forge workspaces (owned ones included) and last-login time. ID, CreatedAt and LastLoginAt
	// are set on u.
	UpsertUser(ctx context.Context, u *User) error
	UserByID(ctx context.Context, id int64) (*User, error)
	ListUsers(ctx context.Context) ([]*User, error)
	// DeleteUser removes the user together with all their sessions.
	DeleteUser(ctx context.Context, id int64) error

	CreateSession(ctx context.Context, sess *Session) error
	// UserBySession resolves a session token hash to its user. Expired
	// sessions are treated as absent (ErrNotFound).
	UserBySession(ctx context.Context, tokenHash string) (*User, error)
	DeleteSession(ctx context.Context, tokenHash string) error

	// CreateUpload persists the upload and its per-file rows atomically,
	// setting u.ID and u.CreatedAt.
	CreateUpload(ctx context.Context, u *Upload, files []*UploadFile) error
	Upload(ctx context.Context, id int64) (*Upload, error)
	// ListUploads returns uploads newest first; limit <= 0 means all.
	ListUploads(ctx context.Context, repoID int64, limit int) ([]*Upload, error)
	// ListBranchUploads is ListUploads restricted to one branch.
	ListBranchUploads(ctx context.Context, repoID int64, branch string, limit int) ([]*Upload, error)
	UploadFiles(ctx context.Context, uploadID int64) ([]*UploadFile, error)
	// LatestUploadsPerPart returns the most recent upload for each distinct
	// part of a commit — the set the merged report is computed from. A
	// re-uploaded part supersedes its earlier uploads here.
	LatestUploadsPerPart(ctx context.Context, repoID int64, commitSHA string) ([]*Upload, error)
	// WithCommitReportTx serializes the recompute of one commit's merged
	// report against concurrent uploads of the same commit and runs fn's
	// reads and upsert as one atomic, locked unit — so a slow recompute can
	// neither interleave with nor clobber a newer one and drop a part. fn
	// must route all its store access through the passed CommitTx. Locks on
	// different commits never contend.
	WithCommitReportTx(ctx context.Context, repoID int64, commitSHA string, fn func(ctx context.Context, tx CommitTx) error) error
	// UpsertCommitReport creates or replaces the merged report for
	// (repo, commit), setting cr.ID, cr.CreatedAt and cr.UpdatedAt. The
	// first-seen creation time is preserved across recomputes.
	UpsertCommitReport(ctx context.Context, cr *CommitReport) error
	// CommitReport returns the merged report for a commit, or ErrNotFound.
	CommitReport(ctx context.Context, repoID int64, commitSHA string) (*CommitReport, error)
	// LatestCommitReport returns the most recent merged report on a branch.
	LatestCommitReport(ctx context.Context, repoID int64, branch string) (*CommitReport, error)
	// LatestNonPRCommitReport is LatestCommitReport restricted to reports
	// that did not come from a pull request build. The badge reads it: a
	// PR whose head branch shares the default branch's name — a fork's
	// "main", say — must not take over the repo's headline number.
	LatestNonPRCommitReport(ctx context.Context, repoID int64, branch string) (*CommitReport, error)
	// LatestPassedCommitReport returns the most recent gate-passing merged
	// report on a branch, skipping excludeCommit (the commit being uploaded,
	// whose own in-progress report must not serve as its baseline). Used as
	// the delta and gate-drop baseline; PR-build reports are excluded — the
	// baseline is always the branch's own history, never a PR that happens
	// to carry the branch's name.
	LatestPassedCommitReport(ctx context.Context, repoID int64, branch, excludeCommit string) (*CommitReport, error)
	// ListBranchCommitReports returns merged reports on a branch newest
	// first; limit <= 0 means all. Feeds the coverage trend.
	ListBranchCommitReports(ctx context.Context, repoID int64, branch string, limit int) ([]*CommitReport, error)
	// TryPushStatus serializes forge status/PR-comment pushes for one commit
	// and runs push only if version is at least the last successfully pushed
	// version, recording version only after push returns nil. It closes the
	// window where a slow older push lands after a newer one, and a failed
	// push leaves the version untouched so a later part retries. push runs
	// with the per-commit lock held and does the forge HTTP itself, bounded
	// by ctx. Returns whether push ran.
	TryPushStatus(ctx context.Context, repoID int64, commitSHA string, version int64, push func(context.Context) error) (pushed bool, err error)
	// CommitParts returns the distinct part names uploaded for a commit —
	// a cheap read (no blocks/diff) for the per-commit parts cap.
	CommitParts(ctx context.Context, repoID int64, commitSHA string) ([]string, error)

	// ClaimTokenlessUpload records that the (workflow run, attempt, part)
	// triple has been accepted for a tokenless upload and reports whether
	// this call won the claim. One accept per triple: a replay of an
	// already-claimed triple returns false and the upload is refused.
	// Concurrent claims of the same triple must resolve to exactly one
	// winner.
	ClaimTokenlessUpload(ctx context.Context, repoID, runID, runAttempt int64, part string) (bool, error)
	// ReleaseTokenlessUpload forgets a claim whose upload then failed to
	// land, so the CI job's retry is not locked out by its own failure.
	ReleaseTokenlessUpload(ctx context.Context, repoID, runID, runAttempt int64, part string) error
}

// CommitTx is the store access available inside WithCommitReportTx. On
// Postgres every call runs on the one locked transaction, so the recompute's
// reads and its upsert are consistent and cannot deadlock on a second pooled
// connection.
type CommitTx interface {
	LatestUploadsPerPart(ctx context.Context, repoID int64, commitSHA string) ([]*Upload, error)
	UploadFiles(ctx context.Context, uploadID int64) ([]*UploadFile, error)
	LatestPassedCommitReport(ctx context.Context, repoID int64, branch, excludeCommit string) (*CommitReport, error)
	UpsertCommitReport(ctx context.Context, cr *CommitReport) error
}

// GrantTx is the store access available inside WithGrantLock. On Postgres
// every call runs on the one locked transaction, for the same reason as
// CommitTx: a refresh that held the lock on one pooled connection and
// reached for a second to read or persist the token would deadlock the
// pool under enough simultaneous cold-cache uploads of one workspace.
type GrantTx interface {
	WorkspaceByPrefix(ctx context.Context, forge, prefix string) (*Workspace, error)
	SetWorkspaceBitbucketGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
	SetWorkspaceGitLabGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error
}
