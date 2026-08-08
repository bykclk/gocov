package postgres_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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

	// Update: branch change + credential clearing round-trips through JSONB,
	// and nullable gate fields survive set/clear cycles.
	minCov, maxDrop := 82.5, 0.0
	repo.DefaultBranch = "develop"
	repo.ForgeCredentials = nil
	repo.Token = "tok-rotated"
	repo.Gate = store.Gate{MinCoverage: &minCov, MaxCoverageDrop: &maxDrop}
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
	if got.Gate.MinCoverage == nil || *got.Gate.MinCoverage != 82.5 ||
		got.Gate.MaxCoverageDrop == nil || *got.Gate.MaxCoverageDrop != 0 ||
		got.Gate.MinDiffCoverage != nil {
		t.Errorf("gate round trip: %+v", got.Gate)
	}
	repo.Gate = store.Gate{}
	if err := st.UpdateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.RepoByID(ctx, repo.ID); got.Gate.Configured() {
		t.Errorf("gate not cleared: %+v", got.Gate)
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

func TestWorkspaceLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	minDiff := 70.0
	w := &store.Workspace{
		Forge: "bitbucket", Prefix: "acme", Token: "ws-tok", DefaultBranch: "development",
		ForgeCredentials: map[string]string{"username": "bot", "app_password": "pw"},
		Gate:             store.Gate{MinDiffCoverage: &minDiff},
	}
	if err := st.CreateWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	if w.ID == 0 || w.CreatedAt.IsZero() {
		t.Fatalf("CreateWorkspace did not fill ID/CreatedAt: %+v", w)
	}

	for name, get := range map[string]func() (*store.Workspace, error){
		"by prefix": func() (*store.Workspace, error) { return st.WorkspaceByPrefix(ctx, "acme") },
		"by token":  func() (*store.Workspace, error) { return st.WorkspaceByToken(ctx, "ws-tok") },
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Prefix != "acme" || got.DefaultBranch != "development" ||
			!reflect.DeepEqual(got.ForgeCredentials, w.ForgeCredentials) {
			t.Errorf("%s: %+v", name, got)
		}
		if got.Gate.MinDiffCoverage == nil || *got.Gate.MinDiffCoverage != 70 {
			t.Errorf("%s gate: %+v", name, got.Gate)
		}
	}

	// Unique constraints.
	if err := st.CreateWorkspace(ctx, &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "other", DefaultBranch: "main"}); err == nil {
		t.Error("duplicate prefix must fail")
	}
	if err := st.CreateWorkspace(ctx, &store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "ws-tok", DefaultBranch: "main"}); err == nil {
		t.Error("duplicate token must fail")
	}

	// List is sorted by prefix.
	if err := st.CreateWorkspace(ctx, &store.Workspace{Forge: "bitbucket", Prefix: "aaa", Token: "tok-2", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListWorkspaces(ctx)
	if err != nil || len(list) != 2 || list[0].Prefix != "aaa" || list[1].Prefix != "acme" {
		t.Errorf("ListWorkspaces = %+v (err %v)", list, err)
	}

	// Update (rotation, credential clearing) and stale-token lookups.
	w.Token = "ws-tok-2"
	w.DefaultBranch = "trunk"
	w.ForgeCredentials = nil
	if err := st.UpdateWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WorkspaceByToken(ctx, "ws-tok"); !errors.Is(err, store.ErrNotFound) {
		t.Error("old token still resolves")
	}
	got, err := st.WorkspaceByPrefix(ctx, "acme")
	if err != nil || got.Token != "ws-tok-2" || got.DefaultBranch != "trunk" {
		t.Errorf("after update: %+v (err %v)", got, err)
	}
	if got.ForgeCredentials != nil {
		t.Errorf("credentials not cleared: %v", got.ForgeCredentials)
	}

	// Delete; missing rows yield ErrNotFound.
	if err := st.DeleteWorkspace(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorkspace(ctx, w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
	if err := st.UpdateWorkspace(ctx, &store.Workspace{ID: 9999, Prefix: "x", Token: "y"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update missing = %v", err)
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

	// Gate-failing uploads round-trip and are excluded from the passing
	// baseline while still being the branch's latest upload.
	failed := &store.Upload{
		RepoID: repo.ID, CommitSHA: "c4", Branch: "main", Format: "go",
		TotalPct: 10, CoveredStmts: 1, TotalStmts: 10, GateFailed: true,
	}
	if err := st.CreateUpload(ctx, failed, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Upload(ctx, failed.ID); err != nil || !got.GateFailed {
		t.Errorf("gate_failed round trip: %+v (err %v)", got, err)
	}
	if latest, err := st.LatestUpload(ctx, repo.ID, "main"); err != nil || latest.ID != failed.ID {
		t.Errorf("LatestUpload = %v, %v (want the failed c4)", latest, err)
	}
	if passed, err := st.LatestPassedUpload(ctx, repo.ID, "main"); err != nil || passed.ID != u2.ID {
		t.Errorf("LatestPassedUpload = %v, %v (want u2, skipping failed c4)", passed, err)
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

func TestUserLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u := &store.User{Forge: "bitbucket", ForgeUUID: "{u1}", Email: "jane@example.com", DisplayName: "Jane Dev",
		ForgeWorkspaces: []string{"acme", "personal"}}
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.CreatedAt.IsZero() || u.LastLoginAt.IsZero() {
		t.Fatalf("UpsertUser did not fill ID/CreatedAt/LastLoginAt: %+v", u)
	}

	// A second login by the same forge account refreshes the same row (R1),
	// including the forge workspace snapshot (M3/D3).
	again := &store.User{Forge: "bitbucket", ForgeUUID: "{u1}", Email: "jane@new.example", DisplayName: "Jane Renamed",
		ForgeWorkspaces: []string{"acme", "newco"}}
	if err := st.UpsertUser(ctx, again); err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID {
		t.Errorf("second login created a new row: %d != %d", again.ID, u.ID)
	}
	got, err := st.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "jane@new.example" || got.DisplayName != "Jane Renamed" {
		t.Errorf("fields not refreshed: %+v", got)
	}
	if !reflect.DeepEqual(got.ForgeWorkspaces, []string{"acme", "newco"}) {
		t.Errorf("forge workspaces not refreshed: %v", got.ForgeWorkspaces)
	}
	if got.LastLoginAt.Before(u.LastLoginAt) {
		t.Errorf("last_login_at went backwards: %v < %v", got.LastLoginAt, u.LastLoginAt)
	}

	// Same UUID under another forge must not collide (R1).
	other := &store.User{Forge: "github", ForgeUUID: "{u1}", Email: "x@example.com", DisplayName: "X"}
	if err := st.UpsertUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if other.ID == u.ID {
		t.Error("uniqueness must be per forge+UUID, not per UUID")
	}

	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers = %d users, %v", len(users), err)
	}

	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByID(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("user survived deletion")
	}
	if err := st.DeleteUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u := &store.User{Forge: "bitbucket", ForgeUUID: "{s1}", Email: "s@example.com", DisplayName: "S"}
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	sess := &store.Session{TokenHash: "hash-live", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if sess.CreatedAt.IsZero() {
		t.Error("CreateSession did not fill CreatedAt")
	}
	got, err := st.UserBySession(ctx, "hash-live")
	if err != nil || got.ID != u.ID {
		t.Fatalf("UserBySession = %v, %v", got, err)
	}

	// R2: a session past expiry never authenticates.
	expired := &store.Session{TokenHash: "hash-expired", UserID: u.ID, ExpiresAt: time.Now().Add(-time.Minute)}
	if err := st.CreateSession(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserBySession(ctx, "hash-expired"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session authenticated: %v", err)
	}

	// R2: logout invalidates server-side immediately.
	if err := st.DeleteSession(ctx, "hash-live"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserBySession(ctx, "hash-live"); !errors.Is(err, store.ErrNotFound) {
		t.Error("deleted session still authenticates")
	}
	if err := st.DeleteSession(ctx, "hash-live"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}

	// R2: deleting a user deletes their sessions.
	sess2 := &store.Session{TokenHash: "hash-cascade", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserBySession(ctx, "hash-cascade"); !errors.Is(err, store.ErrNotFound) {
		t.Error("session survived user deletion")
	}
}

func TestWorkspaceMembership(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u := &store.User{Forge: "bitbucket", ForgeUUID: "{m1}", DisplayName: "Member"}
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	acme := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "tok-acme"}
	beta := &store.Workspace{Forge: "bitbucket", Prefix: "beta", Token: "tok-beta"}
	for _, w := range []*store.Workspace{acme, beta} {
		if err := st.CreateWorkspace(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	prefixes := func() []string {
		t.Helper()
		wss, err := st.ListWorkspacesForUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(wss))
		for i, w := range wss {
			out[i] = w.Prefix
		}
		return out
	}
	memberRows := func() int {
		t.Helper()
		var n int
		if err := st.Pool().QueryRow(ctx,
			`SELECT count(*) FROM workspace_members WHERE user_id = $1`, u.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Initial sync attaches both, ordered by prefix.
	if err := st.SetUserWorkspaces(ctx, u.ID, []int64{beta.ID, acme.ID}); err != nil {
		t.Fatal(err)
	}
	if got := prefixes(); !reflect.DeepEqual(got, []string{"acme", "beta"}) {
		t.Fatalf("after sync: %v, want [acme beta]", got)
	}

	// Re-running with the same set is idempotent (no duplicate rows).
	if err := st.SetUserWorkspaces(ctx, u.ID, []int64{acme.ID, beta.ID}); err != nil {
		t.Fatal(err)
	}
	if n := memberRows(); n != 2 {
		t.Fatalf("idempotent re-sync produced %d rows, want 2", n)
	}

	// Dropping beta on the forge removes only that membership.
	if err := st.SetUserWorkspaces(ctx, u.ID, []int64{acme.ID}); err != nil {
		t.Fatal(err)
	}
	if got := prefixes(); !reflect.DeepEqual(got, []string{"acme"}) {
		t.Fatalf("after drop: %v, want [acme]", got)
	}

	// Deleting the workspace cascades the membership away.
	if err := st.DeleteWorkspace(ctx, acme.ID); err != nil {
		t.Fatal(err)
	}
	if got := prefixes(); len(got) != 0 {
		t.Fatalf("membership survived workspace deletion: %v", got)
	}

	// Re-attach, then delete the user: memberships cascade on that side too.
	if err := st.SetUserWorkspaces(ctx, u.ID, []int64{beta.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if n := memberRows(); n != 0 {
		t.Fatalf("membership survived user deletion: %d rows", n)
	}

	// An empty sync clears everything and is safe on a user with no rows.
	other := &store.User{Forge: "bitbucket", ForgeUUID: "{m2}", DisplayName: "Other"}
	if err := st.UpsertUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserWorkspaces(ctx, other.ID, nil); err != nil {
		t.Fatalf("empty sync on fresh user: %v", err)
	}
}

func TestRegisterWorkspace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u := &store.User{Forge: "bitbucket", ForgeUUID: "{r1}", DisplayName: "Founder"}
	if err := st.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	// Registration creates the workspace and its first membership together.
	w := &store.Workspace{Forge: "bitbucket", Prefix: "startup", Token: "reg-tok", DefaultBranch: "main"}
	if err := st.RegisterWorkspace(ctx, w, u.ID); err != nil {
		t.Fatal(err)
	}
	if w.ID == 0 || w.CreatedAt.IsZero() {
		t.Fatalf("RegisterWorkspace did not fill ID/CreatedAt: %+v", w)
	}
	wss, err := st.ListWorkspacesForUser(ctx, u.ID)
	if err != nil || len(wss) != 1 || wss[0].Prefix != "startup" {
		t.Fatalf("memberships after registration = %v, %v", wss, err)
	}

	// A losing duplicate claim fails atomically: no workspace row, no
	// membership row, and the winner's registration is untouched.
	other := &store.User{Forge: "bitbucket", ForgeUUID: "{r2}", DisplayName: "Latecomer"}
	if err := st.UpsertUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	dup := &store.Workspace{Forge: "bitbucket", Prefix: "startup", Token: "other-tok", DefaultBranch: "main"}
	if err := st.RegisterWorkspace(ctx, dup, other.ID); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	if wss, _ := st.ListWorkspacesForUser(ctx, other.ID); len(wss) != 0 {
		t.Errorf("failed registration left memberships: %v", wss)
	}
	if got, err := st.WorkspaceByPrefix(ctx, "startup"); err != nil || got.Token != "reg-tok" {
		t.Errorf("winner's workspace disturbed: %+v (err %v)", got, err)
	}
}
