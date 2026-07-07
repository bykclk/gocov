package main

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/blobstore"
	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

var tokenRe = regexp.MustCompile(`upload token: ([0-9a-f]{48})`)

func runRepoCmd(t *testing.T, st store.Store, args ...string) (string, error) {
	t.Helper()
	return runRepoCmdBlobs(t, st, blobmem.New(), args...)
}

func runRepoCmdBlobs(t *testing.T, st store.Store, blobs blobstore.Store, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := repoCmd(context.Background(), st, blobs, args, &out)
	return out.String(), err
}

func mustAdd(t *testing.T, st store.Store, args ...string) string {
	t.Helper()
	out, err := runRepoCmd(t, st, append([]string{"add"}, args...)...)
	if err != nil {
		t.Fatalf("repo add: %v", err)
	}
	m := tokenRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no token in output: %s", out)
	}
	return m[1]
}

func TestRepoAdd(t *testing.T) {
	st := storemem.New()
	token := mustAdd(t, st, "-slug", "acme/widgets", "-default-branch", "develop",
		"-bb-username", "u", "-bb-app-password", "p")

	r, err := st.RepoByToken(context.Background(), token)
	if err != nil {
		t.Fatalf("token not resolvable: %v", err)
	}
	if r.Slug != "acme/widgets" || r.DefaultBranch != "develop" || r.Forge != "bitbucket" {
		t.Errorf("repo = %+v", r)
	}
	if r.ForgeCredentials["username"] != "u" || r.ForgeCredentials["app_password"] != "p" {
		t.Errorf("credentials = %v", r.ForgeCredentials)
	}
}

func TestRepoAddValidation(t *testing.T) {
	st := storemem.New()
	tests := []struct {
		name string
		args []string
	}{
		{"missing slug", []string{"add"}},
		{"username without password", []string{"add", "-slug", "a/b", "-bb-username", "u"}},
		{"unknown flag", []string{"add", "-slug", "a/b", "-nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runRepoCmd(t, st, tt.args...); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestFlagErrorsReportedOnce(t *testing.T) {
	st := storemem.New()
	// Parse errors: the flag package writes the message; the returned error
	// must be errPrinted so main does not repeat it.
	out, err := runRepoCmd(t, st, "add", "-nope")
	if !errors.Is(err, errPrinted) {
		t.Errorf("err = %v, want errPrinted", err)
	}
	if !strings.Contains(out, "-nope") {
		t.Errorf("flag error not written to output: %q", out)
	}
	// -h shows usage and succeeds.
	out, err = runRepoCmd(t, st, "add", "-h")
	if err != nil {
		t.Errorf("-h returned error: %v", err)
	}
	if !strings.Contains(out, "-slug") {
		t.Errorf("-h did not print usage: %q", out)
	}
}

func TestRepoList(t *testing.T) {
	st := storemem.New()

	out, err := runRepoCmd(t, st, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no repos registered") {
		t.Errorf("empty list output: %s", out)
	}

	mustAdd(t, st, "-slug", "acme/widgets", "-bb-username", "u", "-bb-app-password", "p")
	mustAdd(t, st, "-slug", "acme/gadgets")

	out, err = runRepoCmd(t, st, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "acme/widgets") || !strings.Contains(out, "acme/gadgets") {
		t.Errorf("list output missing repos: %s", out)
	}
	// Tokens must never be shown by list.
	if tokenRe.MatchString(out) {
		t.Errorf("list output leaks tokens: %s", out)
	}
	// Credentials shown as set/- only.
	widgetsLine := lineContaining(out, "acme/widgets")
	if !strings.Contains(widgetsLine, "set") {
		t.Errorf("widgets line should show credentials as set: %q", widgetsLine)
	}
	if strings.Contains(out, "app_password") || strings.Contains(out, "\tp\t") {
		t.Errorf("list output leaks credentials: %s", out)
	}
}

func lineContaining(s, substr string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

func TestRepoRotateToken(t *testing.T) {
	st := storemem.New()
	oldToken := mustAdd(t, st, "-slug", "acme/widgets")

	out, err := runRepoCmd(t, st, "rotate-token", "-slug", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	m := tokenRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no new token in output: %s", out)
	}
	newToken := m[1]
	if newToken == oldToken {
		t.Fatal("token did not change")
	}

	ctx := context.Background()
	if _, err := st.RepoByToken(ctx, oldToken); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old token still resolves (err=%v)", err)
	}
	r, err := st.RepoByToken(ctx, newToken)
	if err != nil {
		t.Fatalf("new token does not resolve: %v", err)
	}
	if r.Slug != "acme/widgets" {
		t.Errorf("new token resolves to %s", r.Slug)
	}
}

func TestRepoRotateTokenUnknownSlug(t *testing.T) {
	st := storemem.New()
	if _, err := runRepoCmd(t, st, "rotate-token", "-slug", "no/such"); err == nil {
		t.Error("want error for unknown slug")
	}
}

func TestRepoUpdate(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	token := mustAdd(t, st, "-slug", "acme/widgets")

	// Set credentials and change the default branch in one call.
	_, err := runRepoCmd(t, st, "update", "-slug", "acme/widgets",
		"-default-branch", "develop", "-bb-username", "u", "-bb-app-password", "p")
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.RepoBySlug(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if r.DefaultBranch != "develop" || r.ForgeCredentials["username"] != "u" {
		t.Errorf("after update: %+v", r)
	}
	if r.Token != token {
		t.Error("update must not change the token")
	}

	// Clear credentials, leave the branch alone.
	if _, err := runRepoCmd(t, st, "update", "-slug", "acme/widgets", "-clear-credentials"); err != nil {
		t.Fatal(err)
	}
	r, err = st.RepoBySlug(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ForgeCredentials) != 0 {
		t.Errorf("credentials not cleared: %v", r.ForgeCredentials)
	}
	if r.DefaultBranch != "develop" {
		t.Errorf("default branch changed unexpectedly: %s", r.DefaultBranch)
	}
}

func TestRepoUpdateValidation(t *testing.T) {
	st := storemem.New()
	mustAdd(t, st, "-slug", "acme/widgets")
	tests := []struct {
		name string
		args []string
	}{
		{"missing slug", []string{"update", "-default-branch", "x"}},
		{"no changes", []string{"update", "-slug", "acme/widgets"}},
		{"clear and set together", []string{"update", "-slug", "acme/widgets",
			"-clear-credentials", "-bb-username", "u", "-bb-app-password", "p"}},
		{"unknown slug", []string{"update", "-slug", "no/such", "-default-branch", "x"}},
		{"password without username", []string{"update", "-slug", "acme/widgets", "-bb-app-password", "p"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runRepoCmd(t, st, tt.args...); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestRepoRemove(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	blobs := blobmem.New()
	mustAdd(t, st, "-slug", "acme/widgets")
	repo, err := st.RepoBySlug(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, "profiles/1/raw", []byte("mode: set\n")); err != nil {
		t.Fatal(err)
	}
	err = st.CreateUpload(ctx, &store.Upload{
		RepoID: repo.ID, CommitSHA: "c1", Branch: "main", Format: "go", RawBlobKey: "profiles/1/raw",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Without -force: dry run only.
	out, err := runRepoCmdBlobs(t, st, blobs, "remove", "-slug", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would remove") || !strings.Contains(out, "1 upload") {
		t.Errorf("dry run output: %q", out)
	}
	if _, err := st.RepoBySlug(ctx, "acme/widgets"); err != nil {
		t.Fatal("dry run must not delete the repo")
	}

	// With -force: repo, uploads and blobs all gone.
	out, err = runRepoCmdBlobs(t, st, blobs, "remove", "-slug", "acme/widgets", "-force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("output: %q", out)
	}
	if _, err := st.RepoBySlug(ctx, "acme/widgets"); !errors.Is(err, store.ErrNotFound) {
		t.Error("repo still exists")
	}
	if _, err := st.Upload(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Error("upload still exists")
	}
	if _, err := blobs.Get(ctx, "profiles/1/raw"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Error("raw profile blob still exists")
	}
}

func TestRepoRemoveValidation(t *testing.T) {
	st := storemem.New()
	if _, err := runRepoCmd(t, st, "remove", "-force"); err == nil {
		t.Error("want error without slug")
	}
	if _, err := runRepoCmd(t, st, "remove", "-slug", "no/such", "-force"); err == nil {
		t.Error("want error for unknown slug")
	}
}

func TestRepoCmdRejectsStrayArguments(t *testing.T) {
	// flag parsing stops at the first positional; a trailing -force would be
	// silently ignored and the dry run would exit 0. That must be an error.
	ctx := context.Background()
	st := storemem.New()
	mustAdd(t, st, "-slug", "acme/widgets")

	if _, err := runRepoCmd(t, st, "remove", "-slug", "acme/widgets", "stray", "-force"); err == nil {
		t.Fatal("want error for stray positional argument")
	}
	if _, err := st.RepoBySlug(ctx, "acme/widgets"); err != nil {
		t.Error("repo must be untouched")
	}
	if _, err := runRepoCmd(t, st, "list", "stray"); err == nil {
		t.Error("list must reject positional arguments too")
	}
}

func TestRepoCmdUsage(t *testing.T) {
	st := storemem.New()
	if _, err := runRepoCmd(t, st); err == nil {
		t.Error("want usage error with no args")
	}
	if _, err := runRepoCmd(t, st, "bogus"); err == nil {
		t.Error("want error for unknown subcommand")
	}
}
