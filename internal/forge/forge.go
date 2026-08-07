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

// Report results, mapped by each implementation to its native values.
// An empty result means the report carries data without a verdict.
const (
	ReportPassed = "passed"
	ReportFailed = "failed"
)

// Report data value kinds.
const (
	DataPercentage = "percentage" // float64 between 0 and 100
	DataNumber     = "number"     // float64
	DataText       = "text"       // string
)

// ReportData is one key figure shown on a commit report.
type ReportData struct {
	Title string
	Type  string // one of the Data* constants
	Value any    // float64 for percentage/number, string for text
}

// Report is a code-quality report card attached to a commit, surfaced by
// forges next to the pull requests that contain the commit.
type Report struct {
	Title   string
	Details string
	Result  string // ReportPassed, ReportFailed or "" for no verdict
	Link    string // link back to the full report
	Data    []ReportData
}

// Annotation is one finding of a Report, anchored to a source line or,
// when Line is 0, to the file as a whole.
type Annotation struct {
	Path string // repo-relative file path, as it appears in the PR diff
	Line int    // 1-based line in the new file version; 0 = file-level
	// EndLine closes the range a finding spans; 0 or Line for a single
	// line. Forges without range support anchor at Line and ignore it.
	EndLine int
	Summary string
}

// Forge is the VCS-host integration surface used by the server.
type Forge interface {
	// PostBuildStatus writes a build status onto a commit.
	PostBuildStatus(ctx context.Context, repoSlug, commitSHA string, status BuildStatus) error
	// PostPRComment adds a comment to a pull request.
	PostPRComment(ctx context.Context, repoSlug, prID, body string) error
	// FindPRComment returns the id of the newest non-deleted top-level
	// PR comment whose raw content starts with prefix, or "" when there
	// is none. Used to update a previously posted comment in place; a
	// look-alike comment by another author must never capture the slot —
	// either by the implementation matching the credential account, or
	// by the forge rejecting UpdatePRComment on foreign comments (the
	// caller then falls back to posting a fresh comment).
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
	// PublishReport creates or replaces the commit's coverage report
	// together with its annotations. Replace semantics are part of the
	// contract: re-publishing must leave no report duplicates and no
	// annotations from an earlier publish behind.
	PublishReport(ctx context.Context, repoSlug, commitSHA string, report Report, annotations []Annotation) error
}

// Factory builds a Forge from per-repo credentials (as stored in
// repos.forge_credentials). The server holds one Factory per forge name.
type Factory func(credentials map[string]string) (Forge, error)
