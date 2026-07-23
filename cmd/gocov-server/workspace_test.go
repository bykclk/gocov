package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

func runWorkspaceCmd(t *testing.T, st store.Store, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := workspaceCmd(context.Background(), st, args, &out)
	return out.String(), err
}

func mustAddWorkspace(t *testing.T, st store.Store, args ...string) string {
	t.Helper()
	out, err := runWorkspaceCmd(t, st, append([]string{"add"}, args...)...)
	if err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	m := tokenRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no token in output: %s", out)
	}
	return m[1]
}

func TestWorkspaceAddAndList(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	token := mustAddWorkspace(t, st, "-prefix", "acme", "-default-branch", "development")

	w, err := st.WorkspaceByToken(ctx, token)
	if err != nil {
		t.Fatalf("token not resolvable: %v", err)
	}
	if w.Prefix != "acme" || w.DefaultBranch != "development" || w.Forge != "bitbucket" {
		t.Errorf("workspace = %+v", w)
	}

	out, err := runWorkspaceCmd(t, st, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "acme") || !strings.Contains(out, "development") {
		t.Errorf("list output: %q", out)
	}
	if tokenRe.MatchString(out) {
		t.Errorf("list output leaks tokens: %q", out)
	}
}

func TestWorkspaceValidation(t *testing.T) {
	st := storemem.New()
	mustAddWorkspace(t, st, "-prefix", "acme")
	tests := []struct {
		name string
		args []string
	}{
		{"add missing prefix", []string{"add"}},
		{"add duplicate prefix", []string{"add", "-prefix", "acme"}},
		{"rotate missing prefix", []string{"rotate-token"}},
		{"rotate unknown prefix", []string{"rotate-token", "-prefix", "nope"}},
		{"update nothing to change", []string{"update", "-prefix", "acme"}},
		{"update unknown prefix", []string{"update", "-prefix", "nope", "-default-branch", "x"}},
		{"remove missing prefix", []string{"remove", "-force"}},
		{"stray positional", []string{"add", "-prefix", "x", "stray"}},
		{"unknown subcommand", []string{"bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runWorkspaceCmd(t, st, tt.args...); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestWorkspaceRotateToken(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	oldToken := mustAddWorkspace(t, st, "-prefix", "acme")

	out, err := runWorkspaceCmd(t, st, "rotate-token", "-prefix", "acme")
	if err != nil {
		t.Fatal(err)
	}
	m := tokenRe.FindStringSubmatch(out)
	if m == nil || m[1] == oldToken {
		t.Fatalf("rotation output: %q", out)
	}
	if _, err := st.WorkspaceByToken(ctx, oldToken); !errors.Is(err, store.ErrNotFound) {
		t.Error("old workspace token still resolves")
	}
	if _, err := st.WorkspaceByToken(ctx, m[1]); err != nil {
		t.Errorf("new token does not resolve: %v", err)
	}
}

func TestWorkspaceUpdateAndRemove(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	mustAddWorkspace(t, st, "-prefix", "acme")

	if _, err := runWorkspaceCmd(t, st, "update", "-prefix", "acme", "-default-branch", "trunk"); err != nil {
		t.Fatal(err)
	}
	w, err := st.WorkspaceByPrefix(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if w.DefaultBranch != "trunk" {
		t.Errorf("default branch = %q", w.DefaultBranch)
	}

	// Removing the workspace keeps repos: create one as if auto-registered.
	repo := &store.Repo{Forge: "bitbucket", Slug: "acme/app", Token: "t1", DefaultBranch: "trunk"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkspaceCmd(t, st, "remove", "-prefix", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would remove") {
		t.Errorf("dry run output: %q", out)
	}
	if _, err := runWorkspaceCmd(t, st, "remove", "-prefix", "acme", "-force"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WorkspaceByPrefix(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
		t.Error("workspace still exists")
	}
	if _, err := st.RepoBySlug(ctx, "acme/app"); err != nil {
		t.Error("repos must survive workspace removal")
	}
}
