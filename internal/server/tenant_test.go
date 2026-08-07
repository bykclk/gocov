package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/auth"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

// seedRepoUpload registers a repo and one upload with a single file, so the
// repo, upload and source pages all have something to render.
func seedRepoUpload(t *testing.T, f *fixture, slug string) (*store.Repo, *store.Upload) {
	t.Helper()
	ctx := context.Background()
	repo := &store.Repo{Forge: "bitbucket", Slug: slug, Token: "tok-" + slug, DefaultBranch: "main"}
	if err := f.store.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	up := &store.Upload{RepoID: repo.ID, CommitSHA: "deadbeefcafe", Branch: "main", Format: "go",
		TotalPct: 50, CoveredStmts: 1, TotalStmts: 2}
	files := []*store.UploadFile{{Path: "main.go", Pct: 50, CoveredStmts: 1, TotalStmts: 2,
		Blocks: []profile.Block{{StartLine: 1, EndLine: 2, NumStmts: 2, Count: 1}}}}
	if err := f.store.CreateUpload(ctx, up, files); err != nil {
		t.Fatal(err)
	}
	return repo, up
}

// TestTwoTenantIsolation is the demo the milestone exists for: two users in
// different workspaces on one instance see disjoint repos, and a non-member
// deep link 404s rather than leaking existence (D3).
func TestTwoTenantIsolation(t *testing.T) {
	prov := &fakeProvider{identity: memberIdentity()}
	f := newAuthFixture(t, prov, nil) // already tracks acme/widgets
	ctx := context.Background()

	// Workspace rows are the membership anchor (D6). acme covers the
	// fixture's repo; beta is the second tenant.
	acme := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "tok-acme"}
	beta := &store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "tok-beta"}
	for _, w := range []*store.Workspace{acme, beta} {
		if err := f.store.CreateWorkspace(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	_, betaUp := seedRepoUpload(t, f, "beta/gizmos")

	// Two users, each a member of exactly one workspace. The membership is
	// persisted by the login-time sync inside the OAuth callback.
	prov.identity = &auth.Identity{ForgeUUID: "{a}", DisplayName: "Ada", Workspaces: []string{"acme"}}
	ckA := signIn(t, f, "/")
	prov.identity = &auth.Identity{ForgeUUID: "{b}", DisplayName: "Ben", Workspaces: []string{"beta"}}
	ckB := signIn(t, f, "/")

	// Index lists are disjoint.
	if body := get(f, "/", ckA).Body.String(); !strings.Contains(body, "acme/widgets") || strings.Contains(body, "beta/gizmos") {
		t.Errorf("tenant A index leaked or missed a repo:\n%s", body)
	}
	if body := get(f, "/", ckB).Body.String(); !strings.Contains(body, "beta/gizmos") || strings.Contains(body, "acme/widgets") {
		t.Errorf("tenant B index leaked or missed a repo:\n%s", body)
	}

	// Non-member deep links 404 (D3: 404, not 403 — existence stays hidden).
	for _, path := range []string{
		"/repos/beta/gizmos",
		fmt.Sprintf("/uploads/%d", betaUp.ID),
		fmt.Sprintf("/uploads/%d/files/main.go", betaUp.ID),
	} {
		if rec := get(f, path, ckA); rec.Code != http.StatusNotFound {
			t.Errorf("non-member GET %s = %d, want 404", path, rec.Code)
		}
	}

	// The owning tenant still reaches its own pages.
	if rec := get(f, "/repos/beta/gizmos", ckB); rec.Code != http.StatusOK {
		t.Errorf("member GET own repo = %d, want 200", rec.Code)
	}
	if rec := get(f, fmt.Sprintf("/uploads/%d", betaUp.ID), ckB); rec.Code != http.StatusOK {
		t.Errorf("member GET own upload = %d, want 200", rec.Code)
	}
}

// TestOpenModeIgnoresScoping locks in D5: with no sign-in configured the UI
// stays fully open — every repo listed, every deep link reachable.
func TestOpenModeIgnoresScoping(t *testing.T) {
	f := newAuthFixture(t, nil, nil) // auth disabled -> open mode
	_, betaUp := seedRepoUpload(t, f, "beta/gizmos")

	if body := get(f, "/").Body.String(); !strings.Contains(body, "acme/widgets") || !strings.Contains(body, "beta/gizmos") {
		t.Errorf("open mode must list every repo:\n%s", body)
	}
	if rec := get(f, "/repos/beta/gizmos"); rec.Code != http.StatusOK {
		t.Errorf("open mode repo page = %d, want 200", rec.Code)
	}
	if rec := get(f, fmt.Sprintf("/uploads/%d", betaUp.ID)); rec.Code != http.StatusOK {
		t.Errorf("open mode upload page = %d, want 200", rec.Code)
	}
}
