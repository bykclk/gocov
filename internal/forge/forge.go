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
	// GetPRDiff returns the unified diff of a pull request. Needed by the
	// future diff-coverage engine; may return ErrNotImplemented.
	GetPRDiff(ctx context.Context, repoSlug, prID string) (string, error)
}

// Factory builds a Forge from per-repo credentials (as stored in
// repos.forge_credentials). The server holds one Factory per forge name.
type Factory func(credentials map[string]string) (Forge, error)
