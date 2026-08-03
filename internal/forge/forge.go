// Package forge abstracts VCS-host integrations (Bitbucket first; GitHub
// and GitLab later). No forge-specific types or URLs may leak out of the
// concrete implementations.
package forge

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned by forge methods an implementation does not
// support yet.
var ErrNotImplemented = errors.New("forge: not implemented")

// ErrRepoNotFound is returned when the forge reports that a repository
// does not exist (e.g. a 404 while asking for its default branch).
var ErrRepoNotFound = errors.New("forge: repository not found")

// Build status states, mapped by each implementation to its native values.
const (
	StateSuccessful = "successful"
	StateFailed     = "failed"
	StateInProgress = "in_progress"
)

// BuildStatus is a commit build status entry.
type BuildStatus struct {
	Key         string // stable identifier, e.g. "gocov/coverage"
	State       string // one of the State* constants
	Name        string // short human-readable name
	Description string // e.g. "coverage: 87.5% (+1.2%)"
	URL         string // link back to the coverage report
}

// Forge is the VCS-host integration surface used by the server.
type Forge interface {
	// PostBuildStatus writes a build status onto a commit.
	PostBuildStatus(ctx context.Context, repoSlug, commitSHA string, status BuildStatus) error
	// PostPRComment adds a comment to a pull request.
	PostPRComment(ctx context.Context, repoSlug, prID, body string) error
	// FindPRComment returns the id of the newest non-deleted top-level
	// PR comment that was authored by the credential account and whose
	// raw content starts with prefix, or "" when there is none. Used to
	// update a previously posted comment in place; other authors must
	// never match, so a look-alike comment cannot capture the slot.
	FindPRComment(ctx context.Context, repoSlug, prID, prefix string) (string, error)
	// UpdatePRComment replaces the body of an existing PR comment.
	UpdatePRComment(ctx context.Context, repoSlug, prID, commentID, body string) error
	// GetPRDiff returns the unified diff of a pull request.
	GetPRDiff(ctx context.Context, repoSlug, prID string) (string, error)
	// GetDefaultBranch returns the repository's main branch name, used
	// when auto-registering repos on first upload.
	GetDefaultBranch(ctx context.Context, repoSlug string) (string, error)
	// GetFileContent returns a file's raw content at a commit, used by
	// the source view. Returns ErrRepoNotFound-wrapped errors when the
	// file does not exist at that commit.
	GetFileContent(ctx context.Context, repoSlug, commitSHA, path string) ([]byte, error)
}

// Factory builds a Forge from per-repo credentials (as stored in
// repos.forge_credentials). The server holds one Factory per forge name.
type Factory func(credentials map[string]string) (Forge, error)
