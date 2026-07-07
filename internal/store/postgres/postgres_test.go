package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bykclk/gocov/internal/diffcov"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
	"github.com/bykclk/gocov/internal/store/postgres"
	"github.com/bykclk/gocov/internal/testpg"
)

func newTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	st := postgres.New(testpg.Pool(t))
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Migrations must be idempotent across restarts.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	return st
}

func TestRepoLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	repo := &store.Repo{
		Forge:            "bitbucket",
		Slug:             "acme/widgets",
		Token:            "tok-1",
		DefaultBranch:    "main",
		ForgeCredentials: map[string]string{"username": "u", "app_password": "p"},
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if repo.ID == 0 || repo.CreatedAt.IsZero() {
		t.Fatalf("CreateRepo did not fill ID/CreatedAt: %+v", repo)
	}

	// All lookups return the same row, credentials included.
	for name, get := range map[string]func() (*store.Repo, error){
		"by id":    func() (*store.Repo, error) { return st.RepoByID(ctx, repo.ID) },
		"by slug":  func() (*store.Repo, error) { return st.RepoBySlug(ctx, "acme/widgets") },
		"by token": func() (*store.Repo, error) { return st.RepoByToken(ctx, "tok-1") },
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Slug != repo.Slug || got.Token != repo.Token || got.DefaultBranch != "main" ||
			!reflect.DeepEqual(got.ForgeCredentials, repo.ForgeCredentials) {
			t.Errorf("%s: got %+v", name, got)
		}
	}

	// Unique constraints hold.
	dup := &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "other", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, dup); err == nil {
		t.Error("duplicate slug must fail")
	}

	// ListRepos is sorted by slug.
	second := &store.Repo{Forge: "bitbucket", Slug: "aaa/first", Token: "tok-2", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, second); err != nil {
		t.Fatal(err)
	}
	repos, err := st.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Slug != "aaa/first" || repos[1].Slug != "acme/widgets" {
		t.Errorf("ListRepos = %v, %v", repos[0].Slug, repos[1].Slug)
	}

	// Update: branch change + credential clearing round-trips through JSONB.
	repo.DefaultBranch = "develop"
	repo.ForgeCredentials = nil
	repo.Token = "tok-rotated"
	if err := st.UpdateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	got, err := st.RepoByID(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultBranch != "develop" || got.ForgeCredentials != nil || got.Token != "tok-rotated" {
		t.Errorf("after update: %+v", got)
	}
	if _, err := st.RepoByToken(ctx, "tok-1"); !errors.Is(err, store.ErrNotFound) {
		t.Error("old token still resolves after rotation")
	}

	// Missing rows yield ErrNotFound.
	if err := st.UpdateRepo(ctx, &store.Repo{ID: 9999, Slug: "x/y", Token: "t"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateRepo missing = %v", err)
	}
	if _, err := st.RepoBySlug(ctx, "no/such"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RepoBySlug missing = %v", err)
	}
}

func TestUploadLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	repo := &store.Repo{Forge: "bitbucket", Slug: "acme/widgets", Token: "tok", DefaultBranch: "main"}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}

	blocks := []profile.Block{
		{StartLine: 1, StartCol: 1, EndLine: 5, EndCol: 2, NumStmts: 3, Count: 1},
		{StartLine: 7, StartCol: 1, EndLine: 9, EndCol: 2, NumStmts: 2, Count: 0},
	}
	mkUpload := func(commit, branch string, pct float64, dc *diffcov.Result) *store.Upload {
		t.Helper()
		u := &store.Upload{
			RepoID: repo.ID, CommitSHA: commit, Branch: branch, Format: "go",
			TotalPct: pct, CoveredStmts: 3, TotalStmts: 5,
			RawBlobKey: "profiles/" + commit, DiffCoverage: dc,
		}
		files := []*store.UploadFile{
			{Path: "example.com/m/b.go", Pct: 100, CoveredStmts: 2, TotalStmts: 2, Blocks: blocks[:1]},
			{Path: "example.com/m/a.go", Pct: 60, CoveredStmts: 3, TotalStmts: 5, Blocks: blocks},
		}
		if err := st.CreateUpload(ctx, u, files); err != nil {
			t.Fatal(err)
		}
		return u
	}

	u1 := mkUpload("c1", "main", 60, nil)
	u2 := mkUpload("c2", "main", 65, nil)
	dc := &diffcov.Result{
		Files: []diffcov.FileCoverage{
			{Path: "m/a.go", CoveredLines: 2, TotalLines: 3, UncoveredLines: []int{9}},
		},
		CoveredLines: 2, TotalLines: 3,
		UnmatchedFiles: []string{"m/new.go"},
	}
	u3 := mkUpload("c3", "feature/x", 70, dc)

	// Full round trip of a stored upload.
	got, err := st.Upload(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitSHA != "c1" || got.Branch != "main" || got.TotalPct != 60 ||
		got.RawBlobKey != "profiles/c1" || got.DiffCoverage != nil {
		t.Errorf("upload round trip: %+v", got)
	}

	// Per-file rows are sorted and preserve block data exactly.
	files, err := st.UploadFiles(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "example.com/m/a.go" {
		t.Fatalf("files = %+v", files)
	}
	if !reflect.DeepEqual(files[0].Blocks, blocks) {
		t.Errorf("blocks round trip: %+v", files[0].Blocks)
	}

	// Diff coverage round-trips through JSONB.
	got3, err := st.Upload(ctx, u3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got3.DiffCoverage, dc) {
		t.Errorf("diff coverage round trip:\n got %+v\nwant %+v", got3.DiffCoverage, dc)
	}

	// LatestUpload is per branch.
	if latest, err := st.LatestUpload(ctx, repo.ID, "main"); err != nil || latest.ID != u2.ID {
		t.Errorf("latest main = %v, %v (want u2)", latest, err)
	}
	if latest, err := st.LatestUpload(ctx, repo.ID, "feature/x"); err != nil || latest.ID != u3.ID {
		t.Errorf("latest feature = %v, %v (want u3)", latest, err)
	}
	if _, err := st.LatestUpload(ctx, repo.ID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("latest missing branch = %v", err)
	}

	// ListUploads: newest first, limited and unlimited.
	ups, err := st.ListUploads(ctx, repo.ID, 2)
	if err != nil || len(ups) != 2 || ups[0].ID != u3.ID || ups[1].ID != u2.ID {
		t.Errorf("limited list = %v (err %v)", ups, err)
	}
	ups, err = st.ListUploads(ctx, repo.ID, 0)
	if err != nil || len(ups) != 3 {
		t.Errorf("unlimited list = %d uploads (err %v)", len(ups), err)
	}

	// DeleteRepo cascades to uploads and files.
	if err := st.DeleteRepo(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Upload(ctx, u1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("upload survived repo deletion")
	}
	if _, err := st.RepoByID(ctx, repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("repo survived deletion")
	}
	if err := st.DeleteRepo(ctx, repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}
