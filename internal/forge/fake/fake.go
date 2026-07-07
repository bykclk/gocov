// Package fake provides a recording forge.Forge test double.
package fake

import (
	"context"
	"sync"

	"github.com/bykclk/gocov/internal/forge"
)

// StatusCall records one PostBuildStatus invocation.
type StatusCall struct {
	RepoSlug  string
	CommitSHA string
	Status    forge.BuildStatus
}

// CommentCall records one PostPRComment invocation.
type CommentCall struct {
	RepoSlug string
	PRID     string
	Body     string
}

// Forge records calls and returns configurable errors.
type Forge struct {
	mu sync.Mutex

	StatusErr  error  // returned by PostBuildStatus
	CommentErr error  // returned by PostPRComment
	DiffText   string // returned by GetPRDiff; empty means ErrNotImplemented
	DiffErr    error  // returned by GetPRDiff when set

	StatusCalls  []StatusCall
	CommentCalls []CommentCall
	DiffCalls    []DiffCall
}

// DiffCall records one GetPRDiff invocation.
type DiffCall struct {
	RepoSlug string
	PRID     string
}

// New returns an empty fake forge.
func New() *Forge { return &Forge{} }

// Factory returns a forge.Factory that always yields f and records the
// credentials it was invoked with.
func (f *Forge) Factory() forge.Factory {
	return func(creds map[string]string) (forge.Forge, error) {
		return f, nil
	}
}

func (f *Forge) PostBuildStatus(_ context.Context, repoSlug, commitSHA string, status forge.BuildStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return f.StatusErr
	}
	f.StatusCalls = append(f.StatusCalls, StatusCall{repoSlug, commitSHA, status})
	return nil
}

func (f *Forge) PostPRComment(_ context.Context, repoSlug, prID, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommentErr != nil {
		return f.CommentErr
	}
	f.CommentCalls = append(f.CommentCalls, CommentCall{repoSlug, prID, body})
	return nil
}

func (f *Forge) GetPRDiff(_ context.Context, repoSlug, prID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DiffCalls = append(f.DiffCalls, DiffCall{repoSlug, prID})
	if f.DiffErr != nil {
		return "", f.DiffErr
	}
	if f.DiffText == "" {
		return "", forge.ErrNotImplemented
	}
	return f.DiffText, nil
}

var _ forge.Forge = (*Forge)(nil)
