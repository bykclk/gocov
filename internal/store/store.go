// Package store defines the storage interface and its domain types.
// Implementations: postgres (production), memory (tests).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/bykclk/gocov/internal/diffcov"
	"github.com/bykclk/gocov/internal/profile"
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
	Gate      Gate
	CreatedAt time.Time
}

// Repo is a tracked repository. Slug is namespaced ("workspace/repo").
type Repo struct {
	ID            int64
	Forge         string // "bitbucket" for now
	Slug          string
	Token         string // per-repo upload token
	DefaultBranch string
	// ForgeCredentials holds forge-specific secrets (e.g. bitbucket
	// username/app_password). Nil or empty when not configured.
	ForgeCredentials map[string]string
	// Gate holds the repo's coverage requirements; violations turn the
	// pushed build status into a failure.
	Gate      Gate
	CreatedAt time.Time
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
	CreatedAt  time.Time
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
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// Session is a server-side web UI session. Only a hash of the session
// token is stored, never the token itself.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
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
	// UpdateRepo replaces the stored row matching r.ID with r's fields.
	UpdateRepo(ctx context.Context, r *Repo) error
	// DeleteRepo removes a repo together with its uploads and per-file rows.
	// Raw profile blobs are not touched; callers clean those up first.
	DeleteRepo(ctx context.Context, id int64) error
	RepoByID(ctx context.Context, id int64) (*Repo, error)
	RepoBySlug(ctx context.Context, slug string) (*Repo, error)
	RepoByToken(ctx context.Context, token string) (*Repo, error)
	ListRepos(ctx context.Context) ([]*Repo, error)

	CreateWorkspace(ctx context.Context, w *Workspace) error
	// UpdateWorkspace replaces the stored row matching w.ID with w's fields.
	UpdateWorkspace(ctx context.Context, w *Workspace) error
	// DeleteWorkspace removes the workspace token; repos created through
	// it are left untouched.
	DeleteWorkspace(ctx context.Context, id int64) error
	WorkspaceByPrefix(ctx context.Context, prefix string) (*Workspace, error)
	WorkspaceByToken(ctx context.Context, token string) (*Workspace, error)
	ListWorkspaces(ctx context.Context) ([]*Workspace, error)

	// SetUserWorkspaces replaces a user's workspace memberships with the
	// given set: memberships not listed are removed and listed ones are
	// added, so re-running it with the same IDs is a no-op. Called at login
	// to mirror the user's current forge membership (M2).
	SetUserWorkspaces(ctx context.Context, userID int64, workspaceIDs []int64) error
	// ListWorkspacesForUser returns the workspaces the user is a member of,
	// ordered by prefix.
	ListWorkspacesForUser(ctx context.Context, userID int64) ([]*Workspace, error)

	// UpsertUser creates the user on first login or, when a row with the
	// same forge+ForgeUUID exists, refreshes its email, display name and
	// last-login time. ID, CreatedAt and LastLoginAt are set on u.
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
	// LatestUpload returns the most recent upload for a branch.
	LatestUpload(ctx context.Context, repoID int64, branch string) (*Upload, error)
	// LatestPassedUpload returns the most recent upload for a branch that
	// did not fail the coverage gate; used as a comparison baseline.
	LatestPassedUpload(ctx context.Context, repoID int64, branch string) (*Upload, error)
}
