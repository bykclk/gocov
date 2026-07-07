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
	CreatedAt        time.Time
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
	CreatedAt    time.Time
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

	// CreateUpload persists the upload and its per-file rows atomically,
	// setting u.ID and u.CreatedAt.
	CreateUpload(ctx context.Context, u *Upload, files []*UploadFile) error
	Upload(ctx context.Context, id int64) (*Upload, error)
	// ListUploads returns uploads newest first; limit <= 0 means all.
	ListUploads(ctx context.Context, repoID int64, limit int) ([]*Upload, error)
	UploadFiles(ctx context.Context, uploadID int64) ([]*UploadFile, error)
	// LatestUpload returns the most recent upload for a branch.
	LatestUpload(ctx context.Context, repoID int64, branch string) (*Upload, error)
}
