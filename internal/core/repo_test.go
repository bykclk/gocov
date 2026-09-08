package core

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gocov/gocov/internal/forge"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

func TestValidRepoName(t *testing.T) {
	tests := []struct {
		forge, name string
		want        bool
	}{
		{"bitbucket", "widgets", true},
		{"bitbucket", "sub/widgets", false},
		{"github", "sub/widgets", false},
		{"gitlab", "widgets", true},
		{"gitlab", "sub/widgets", true},
		{"gitlab", "sub/team/widgets", true},
		{"gitlab", "sub//widgets", false},
		{"gitlab", "sub/../widgets", false},
		{"gitlab", "", false},
	}
	for _, tt := range tests {
		if got := ValidRepoName(tt.forge, tt.name); got != tt.want {
			t.Errorf("ValidRepoName(%q, %q) = %v, want %v", tt.forge, tt.name, got, tt.want)
		}
	}
}

func TestRefreshVisibilityCachesForgeAnswer(t *testing.T) {
	p, st, repo := newPipeline(t, store.Gate{})
	ctx := t.Context()

	fg := forgefake.New()
	fg.Visibility = forge.VisibilityPublic
	p.RefreshVisibility(ctx, fg, repo)
	if repo.Visibility != store.VisibilityPublic {
		t.Errorf("repo.Visibility = %q, want public", repo.Visibility)
	}
	stored, err := st.RepoBySlug(ctx, repo.Forge, repo.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Visibility != store.VisibilityPublic {
		t.Errorf("stored visibility = %q, want public", stored.Visibility)
	}
	if stored.VisibilityCheckedAt.IsZero() {
		t.Error("a definitive answer did not stamp VisibilityCheckedAt")
	}

	// An unchanged answer still refreshes the stamp — the freshness
	// windows count from the last answer, not the last change.
	old := time.Unix(1, 0)
	repo.VisibilityCheckedAt = old
	p.RefreshVisibility(ctx, fg, repo)
	if !repo.VisibilityCheckedAt.After(old) {
		t.Error("an unchanged answer did not refresh the checked-at stamp")
	}

	// Flipped private on the forge: the cache follows.
	fg.Visibility = forge.VisibilityPrivate
	p.RefreshVisibility(ctx, fg, repo)
	if stored, _ := st.RepoBySlug(ctx, repo.Forge, repo.Slug); stored.Visibility != store.VisibilityPrivate {
		t.Errorf("stored visibility after flip = %q, want private", stored.Visibility)
	}

	// A forge that cannot answer keeps the last known state — and the
	// old stamp; so does having no forge at all, and so does an answer
	// outside the public/private contract — it is rejected, never cached
	// verbatim.
	repo.VisibilityCheckedAt = old
	fg.VisibilityErr = errors.New("forge down")
	p.RefreshVisibility(ctx, fg, repo)
	p.RefreshVisibility(ctx, nil, repo)
	fg.VisibilityErr = nil
	fg.Visibility = "internal"
	p.RefreshVisibility(ctx, fg, repo)
	if stored, _ := st.RepoBySlug(ctx, repo.Forge, repo.Slug); stored.Visibility != store.VisibilityPrivate {
		t.Errorf("stored visibility after failures = %q, want private", stored.Visibility)
	}
	if !repo.VisibilityCheckedAt.Equal(old) {
		t.Error("a failed refresh advanced the checked-at stamp")
	}
}

func TestRefreshVisibilityFailsClosedWhenRepoIsGone(t *testing.T) {
	p, st, repo := newPipeline(t, store.Gate{})
	ctx := t.Context()

	fg := forgefake.New()
	fg.Visibility = forge.VisibilityPublic
	p.RefreshVisibility(ctx, fg, repo)

	// The forge positively saying the repo is gone (deleted, or hidden
	// from a connection that lost access) is a definitive answer, not a
	// transient failure: certainly not public any more.
	fg.VisibilityErr = fmt.Errorf("wrapped: %w", forge.ErrRepoNotFound)
	p.RefreshVisibility(ctx, fg, repo)
	if repo.Visibility != store.VisibilityPrivate {
		t.Errorf("repo.Visibility after not-found = %q, want private", repo.Visibility)
	}
	if stored, _ := st.RepoBySlug(ctx, repo.Forge, repo.Slug); stored.Visibility != store.VisibilityPrivate {
		t.Errorf("stored visibility after not-found = %q, want private", stored.Visibility)
	}
}

// TestReverifyVisibilityIfStale covers the serving-path half of the
// staleness mechanism: a stale cached answer is re-checked through the
// repo's forge connection in the background, a fresh one is not, and
// attempts are rate-limited per repo.
func TestReverifyVisibilityIfStale(t *testing.T) {
	ctx := t.Context()
	ff := forgefake.New()
	ff.Visibility = forge.VisibilityPrivate
	forges, st := newForges(t, &fakeBB{client: ff})
	connectedWorkspace(t, st, "acme")
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "tok-vis",
		DefaultBranch: "main", Visibility: store.VisibilityPublic,
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{Store: st, Log: forges.Log, Forges: forges}

	// A zero stamp is maximally stale: the re-check starts and, the forge
	// now answering private, closes the repo.
	if !p.ReverifyVisibilityIfStale(repo) {
		t.Fatal("stale answer did not start a re-check")
	}
	waitForVisibility(t, st, repo.Slug, store.VisibilityPrivate)

	// A second attempt within the rate-limit gap is refused, however
	// stale the caller's struct still looks.
	if p.ReverifyVisibilityIfStale(repo) {
		t.Error("second attempt within the gap was not rate-limited")
	}

	// A fresh stamp never starts a check, and neither does a pipeline
	// without forge connections.
	fresh := *repo
	fresh.ID = repo.ID + 1000 // dodge the rate limiter: freshness must refuse first
	fresh.VisibilityCheckedAt = time.Now()
	if p.ReverifyVisibilityIfStale(&fresh) {
		t.Error("fresh answer started a re-check")
	}
	if (&Pipeline{Store: st, Log: forges.Log}).ReverifyVisibilityIfStale(repo) {
		t.Error("pipeline without Forges started a re-check")
	}
}

// TestSetRepoVisibilityIgnoresStaleAnswers pins the memory double to the
// Store contract's answer ordering: a write whose ask predates the stored
// stamp lost the race and must be skipped, or an in-flight refresh could
// overwrite a fresher webhook-delivered flip.
func TestSetRepoVisibilityIgnoresStaleAnswers(t *testing.T) {
	_, st, repo := newPipeline(t, store.Gate{})
	ctx := t.Context()

	now := time.Now()
	if err := st.SetRepoVisibility(ctx, repo.ID, store.VisibilityPrivate, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoVisibility(ctx, repo.ID, store.VisibilityPublic, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if stored, _ := st.RepoBySlug(ctx, repo.Forge, repo.Slug); stored.Visibility != store.VisibilityPrivate {
		t.Errorf("a stale answer overwrote a fresher one: %q", stored.Visibility)
	}
	if err := st.SetRepoVisibility(ctx, repo.ID, store.VisibilityPublic, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if stored, _ := st.RepoBySlug(ctx, repo.Forge, repo.Slug); stored.Visibility != store.VisibilityPublic {
		t.Errorf("a fresher answer was refused: %q", stored.Visibility)
	}
}

// TestVisibilityRefreshInFlightGuard covers the per-repo claim that keeps
// a commit's concurrently uploading parts from all asking the forge the
// same visibility question.
func TestVisibilityRefreshInFlightGuard(t *testing.T) {
	p := &Pipeline{}
	if !p.beginVisibilityRefresh(1) {
		t.Fatal("first claim refused")
	}
	if p.beginVisibilityRefresh(1) {
		t.Error("concurrent claim for the same repo allowed")
	}
	if !p.beginVisibilityRefresh(2) {
		t.Error("an unrelated repo was blocked")
	}
	p.endVisibilityRefresh(1)
	if !p.beginVisibilityRefresh(1) {
		t.Error("claim after release refused")
	}
}

// waitForVisibility polls the store until the repo's cached visibility
// matches — the background re-check runs on its own goroutine.
func waitForVisibility(t *testing.T, st *storemem.Store, slug, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, err := st.RepoBySlug(t.Context(), "bitbucket", slug)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Visibility == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("visibility = %q, want %q (background re-check never landed)", stored.Visibility, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
