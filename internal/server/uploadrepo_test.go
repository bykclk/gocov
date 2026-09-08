package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/forge"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

func TestWorkspaceTokenUpload(t *testing.T) {
	ctx := t.Context()
	newWsFixture := func(t *testing.T, wsDefaultBranch, forgeDefaultBranch string, connected bool) *fixture {
		t.Helper()
		st := storemem.New()
		ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-token", DefaultBranch: wsDefaultBranch}
		if err := st.CreateWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
		ff := forgefake.New()
		ff.DefaultBranch = forgeDefaultBranch
		cfg := Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
		}
		// A one-click Bitbucket connection is what makes the forge askable
		// for a repo-less auto-create; without it no forge client is built.
		if connected {
			if err := st.SetWorkspaceBitbucketGrant(ctx, ws.ID, "covbot", "rt-0", false); err != nil {
				t.Fatal(err)
			}
			cfg.BitbucketConnect = &fakeBBConnect{grantForge: ff}
		}
		return &fixture{srv: New(cfg), store: st, forge: ff}
	}

	t.Run("auto-creates repo with forge default branch", func(t *testing.T) {
		f := newWsFixture(t, "develop", "development", true)
		rec := doUpload(t, f, "ws-token", map[string]string{
			"repo": "acme/newrepo", "commit": "c1", "branch": "development",
		}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.RepoCreated {
			t.Error("repo_created not reported")
		}
		repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/newrepo")
		if err != nil {
			t.Fatal(err)
		}
		if repo.DefaultBranch != "development" {
			t.Errorf("default branch = %q, want development (from forge)", repo.DefaultBranch)
		}
		if repo.Token == "" || repo.Token == "ws-token" {
			t.Errorf("auto-created repo must get its own token, got %q", repo.Token)
		}
		if len(f.forge.DefaultBranchCalls) != 1 || f.forge.DefaultBranchCalls[0] != "acme/newrepo" {
			t.Errorf("default branch calls = %v", f.forge.DefaultBranchCalls)
		}

		// Second upload reuses the repo.
		rec = doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c2"}, testProfile)
		var resp2 uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp2); err != nil {
			t.Fatal(err)
		}
		if resp2.RepoCreated {
			t.Error("second upload must not report repo_created")
		}
		repos, _ := f.store.ListRepos(ctx)
		if len(repos) != 1 {
			t.Errorf("got %d repos, want 1", len(repos))
		}
	})

	t.Run("falls back to workspace default branch without forge", func(t *testing.T) {
		// No credentials: the forge cannot be asked.
		f := newWsFixture(t, "develop", "development", false)
		doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c1"}, testProfile)
		repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/newrepo")
		if err != nil {
			t.Fatal(err)
		}
		if repo.DefaultBranch != "develop" {
			t.Errorf("default branch = %q, want develop (workspace fallback)", repo.DefaultBranch)
		}
		if len(f.forge.DefaultBranchCalls) != 0 {
			t.Error("forge must not be asked without credentials")
		}
	})

	t.Run("falls back to main when forge has no answer", func(t *testing.T) {
		// Credentials exist but the fake forge returns ErrNotImplemented,
		// and the workspace has no default of its own.
		f := newWsFixture(t, "", "", true)
		doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c1"}, testProfile)
		repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/newrepo")
		if err != nil {
			t.Fatal(err)
		}
		if repo.DefaultBranch != "main" {
			t.Errorf("default branch = %q, want main (last resort)", repo.DefaultBranch)
		}
	})

	t.Run("validation", func(t *testing.T) {
		f := newWsFixture(t, "", "", false)
		tests := []struct {
			name   string
			fields map[string]string
			want   int
		}{
			{"missing repo field", map[string]string{"commit": "c"}, http.StatusBadRequest},
			{"slug outside workspace", map[string]string{"repo": "other/repo", "commit": "c"}, http.StatusForbidden},
			{"no slash", map[string]string{"repo": "acme", "commit": "c"}, http.StatusForbidden},
			{"prefix only", map[string]string{"repo": "acme/", "commit": "c"}, http.StatusBadRequest},
			{"trailing slash", map[string]string{"repo": "acme/widgets/", "commit": "c"}, http.StatusBadRequest},
			{"multi segment", map[string]string{"repo": "acme/a/b", "commit": "c"}, http.StatusBadRequest},
			{"path traversal", map[string]string{"repo": "acme/../victim", "commit": "c"}, http.StatusBadRequest},
			{"overlong name", map[string]string{"repo": "acme/" + strings.Repeat("x", 101), "commit": "c"}, http.StatusBadRequest},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rec := doUpload(t, f, "ws-token", tt.fields, testProfile)
				if rec.Code != tt.want {
					t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body)
				}
			})
		}
		if repos, _ := f.store.ListRepos(ctx); len(repos) != 0 {
			t.Errorf("rejected uploads must not create repos, got %v", repos)
		}
	})

	t.Run("forge 404 blocks auto-registration", func(t *testing.T) {
		f := newWsFixture(t, "develop", "", true)
		f.forge.DefaultBranchErr = forge.ErrRepoNotFound
		rec := doUpload(t, f, "ws-token", map[string]string{"repo": "acme/ghost", "commit": "c"}, testProfile)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body)
		}
		if _, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/ghost"); !errors.Is(err, store.ErrNotFound) {
			t.Error("nonexistent forge repo must not be registered")
		}
	})

	t.Run("transient forge error falls back instead of blocking", func(t *testing.T) {
		f := newWsFixture(t, "develop", "", true)
		f.forge.DefaultBranchErr = errFake
		rec := doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c"}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		repo, err := f.store.RepoBySlug(ctx, "bitbucket", "acme/newrepo")
		if err != nil {
			t.Fatal(err)
		}
		if repo.DefaultBranch != "develop" {
			t.Errorf("default branch = %q, want develop (workspace fallback)", repo.DefaultBranch)
		}
	})

	t.Run("repo token still works and wins", func(t *testing.T) {
		f := newWsFixture(t, "", "", false)
		repo := &store.Repo{Forge: "bitbucket", Slug: "acme/existing", Token: "repo-token", DefaultBranch: "main"}
		if err := f.store.CreateRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
		rec := doUpload(t, f, "repo-token", map[string]string{"commit": "c"}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Errorf("repo token upload failed: %d", rec.Code)
		}
	})
}

func TestGitLabNestedWorkspaceUpload(t *testing.T) {
	// GitLab namespaces nest (D2): the workspace prefix is the registered
	// namespace path — possibly a subgroup — and project slugs may carry
	// further subgroup segments below it.
	ctx := t.Context()
	newGLFixture := func(t *testing.T, prefix string) *fixture {
		t.Helper()
		st := storemem.New()
		ws := &store.Workspace{Forge: "gitlab", Prefix: prefix, Token: "ws-token", DefaultBranch: "main"}
		if err := st.CreateWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
		ff := forgefake.New()
		cfg := Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			BaseURL: "https://gocov.example",
		}
		return &fixture{srv: New(cfg), store: st, forge: ff}
	}

	t.Run("subgroup workspace accepts its projects", func(t *testing.T) {
		f := newGLFixture(t, "grp/sub")
		rec := doUpload(t, f, "ws-token", map[string]string{
			"repo": "grp/sub/proj", "commit": "c1",
		}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if _, err := f.store.RepoBySlug(ctx, "gitlab", "grp/sub/proj"); err != nil {
			t.Errorf("repo not auto-registered: %v", err)
		}
	})

	t.Run("project deeper below the workspace", func(t *testing.T) {
		f := newGLFixture(t, "grp")
		rec := doUpload(t, f, "ws-token", map[string]string{
			"repo": "grp/sub/team/proj", "commit": "c1",
		}, testProfile)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("segment boundary is enforced", func(t *testing.T) {
		// "grp/subx" must not pass as being under workspace "grp/sub".
		f := newGLFixture(t, "grp/sub")
		rec := doUpload(t, f, "ws-token", map[string]string{
			"repo": "grp/subx/proj", "commit": "c1",
		}, testProfile)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403; body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("nested name segments are validated", func(t *testing.T) {
		f := newGLFixture(t, "grp")
		for _, slug := range []string{"grp/sub/../victim", "grp/sub//proj", "grp/sub/"} {
			rec := doUpload(t, f, "ws-token", map[string]string{
				"repo": slug, "commit": "c1",
			}, testProfile)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400; body = %s", slug, rec.Code, rec.Body)
			}
		}
		if repos, _ := f.store.ListRepos(ctx); len(repos) != 0 {
			t.Errorf("rejected uploads must not create repos, got %v", repos)
		}
	})
}
