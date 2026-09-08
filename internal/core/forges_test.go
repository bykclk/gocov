package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gocov/gocov/internal/forge"
	bbforge "github.com/gocov/gocov/internal/forge/bitbucket"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

// fakeBB is a Bitbucket connector whose refreshes are counted and whose
// answers are scripted, so the tests can see how often a live token costs
// a round trip and what happens when the grant is gone.
type fakeBB struct {
	client    forge.Forge
	refreshes int
	err       error
	ttl       time.Duration
}

func (f *fakeBB) AuthorizeURL(state, redirectURI string) string {
	return "https://bb.example/authorize"
}
func (f *fakeBB) Exchange(ctx context.Context, code, redirectURI string) (*bbforge.Grant, error) {
	return nil, errors.New("not used")
}
func (f *fakeBB) Refresh(ctx context.Context, refreshToken string) (*bbforge.Grant, error) {
	f.refreshes++
	if f.err != nil {
		return nil, f.err
	}
	ttl := f.ttl
	if ttl == 0 {
		ttl = time.Hour
	}
	// Bitbucket rotates the refresh token on every use.
	return &bbforge.Grant{Account: "covbot", AccessToken: "at-1", RefreshToken: "rt-next", TTL: ttl}, nil
}
func (f *fakeBB) ForgeClient(accessToken string) forge.Forge { return f.client }

func newForges(t *testing.T, bb BitbucketConnect) (*Forges, *storemem.Store) {
	t.Helper()
	st := storemem.New()
	return NewForges(st, slog.New(slog.NewTextHandler(io.Discard, nil)), "https://cov.example.com", nil, bb, nil), st
}

func connectedWorkspace(t *testing.T, st *storemem.Store, prefix string) *store.Workspace {
	t.Helper()
	ctx := t.Context()
	ws := &store.Workspace{Forge: "bitbucket", Prefix: prefix, Token: "ws-tok", DefaultBranch: "main"}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkspaceBitbucketGrant(ctx, ws.ID, "covbot", "rt-0", false); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.WorkspaceByPrefix(ctx, "bitbucket", prefix)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func TestGrantTokenIsCachedBetweenUploads(t *testing.T) {
	bb := &fakeBB{client: forgefake.New()}
	f, st := newForges(t, bb)
	ws := connectedWorkspace(t, st, "acme")
	ctx := t.Context()

	for i := range 3 {
		if fg := f.Connected(ctx, ws, "bitbucket"); fg == nil {
			t.Fatalf("call %d: no forge client", i)
		}
	}
	if bb.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1: the access token should be cached", bb.refreshes)
	}

	// Disconnecting must not leave a usable token behind in memory.
	f.DropGrantToken("bitbucket", ws.ID)
	if fg := f.Connected(ctx, ws, "bitbucket"); fg == nil {
		t.Fatal("no client after the cache was dropped")
	}
	if bb.refreshes != 2 {
		t.Errorf("refreshes = %d, want 2 after dropping the cached token", bb.refreshes)
	}
}

func TestConcurrentUploadsShareOneRefresh(t *testing.T) {
	// A cold cache and a burst of uploads for one workspace: every
	// request misses the cache at once, and only the first to take the
	// grant lock may spend a refresh — the rest must find the token it
	// cached, not each rotate the refresh token in turn and hand out a
	// token minted from a stale one.
	bb := &fakeBB{client: forgefake.New()}
	f, st := newForges(t, bb)
	ws := connectedWorkspace(t, st, "acme")
	ctx := t.Context()

	const n = 16
	var wg sync.WaitGroup
	clients := make([]forge.Forge, n)
	for i := range n {
		wg.Go(func() { clients[i] = f.Connected(ctx, ws, "bitbucket") })
	}
	wg.Wait()
	for i, c := range clients {
		if c == nil {
			t.Errorf("upload %d: no forge client", i)
		}
	}
	if bb.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1: the lock should let one refresh serve the burst", bb.refreshes)
	}
}

func TestRotatedRefreshTokenIsPersisted(t *testing.T) {
	// Every refresh invalidates the stored token, so losing the new one
	// breaks the next refresh: it has to reach the store before the
	// access token is handed out.
	bb := &fakeBB{client: forgefake.New()}
	f, st := newForges(t, bb)
	ws := connectedWorkspace(t, st, "acme")

	if fg := f.Connected(t.Context(), ws, "bitbucket"); fg == nil {
		t.Fatal("no forge client")
	}
	fresh, err := st.WorkspaceByPrefix(t.Context(), "bitbucket", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.BitbucketRefreshToken != "rt-next" {
		t.Errorf("stored refresh token = %q, want the rotated %q", fresh.BitbucketRefreshToken, "rt-next")
	}
}

func TestRevokedGrantIsMarkedBroken(t *testing.T) {
	bb := &fakeBB{err: forge.ErrCredentialsRevoked}
	f, st := newForges(t, bb)
	ws := connectedWorkspace(t, st, "acme")
	ctx := t.Context()

	if fg := f.Connected(ctx, ws, "bitbucket"); fg != nil {
		t.Error("a revoked grant handed out a client")
	}
	fresh, err := st.WorkspaceByPrefix(ctx, "bitbucket", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.BitbucketGrantBroken {
		t.Error("the grant was not marked broken, so settings will not ask for a reconnect")
	}
	// The account is kept: it says who has to reconnect.
	if fresh.BitbucketGrantAccount != "covbot" {
		t.Errorf("account = %q, want it kept", fresh.BitbucketGrantAccount)
	}
}

func TestCapableFollowsTheConfiguredConnectors(t *testing.T) {
	f, _ := newForges(t, &fakeBB{})
	for forgeName, want := range map[string]bool{"bitbucket": true, "github": false, "gitlab": false, "gitea": false} {
		if got := f.Capable(forgeName); got != want {
			t.Errorf("Capable(%q) = %v, want %v", forgeName, got, want)
		}
	}
	bare, _ := newForges(t, nil)
	if bare.Capable("bitbucket") {
		t.Error("Capable = true with no connector configured")
	}
}

func TestWorkspaceForPrefersTheLongestPrefix(t *testing.T) {
	// A subgroup registered on its own must win over its parent: its
	// repos were onboarded with the subgroup's connection.
	f, st := newForges(t, nil)
	ctx := t.Context()
	for _, prefix := range []string{"acme", "acme/team"} {
		if err := st.CreateWorkspace(ctx, &store.Workspace{
			Forge: "gitlab", Prefix: prefix, Token: "tok-" + prefix, DefaultBranch: "main",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := f.WorkspaceFor(ctx, "acme/team/widgets", "gitlab")
	if got == nil || got.Prefix != "acme/team" {
		t.Fatalf("workspace = %v, want the subgroup acme/team", got)
	}
}

func TestWorkspaceForRefusesAnotherForge(t *testing.T) {
	// Prefixes are globally unique, so a workspace named "acme" on one
	// forge must never lend its grant to a same-named account on another.
	f, st := newForges(t, nil)
	ctx := t.Context()
	if err := st.CreateWorkspace(ctx, &store.Workspace{
		Forge: "gitlab", Prefix: "acme", Token: "tok", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.WorkspaceFor(ctx, "acme/widgets", "github"); got != nil {
		t.Errorf("a github repo borrowed the gitlab workspace %q", got.Prefix)
	}
	if got := f.WorkspaceFor(ctx, "acme/widgets", "gitlab"); got == nil {
		t.Error("the gitlab repo did not find its own workspace")
	}
	if got := f.WorkspaceFor(ctx, "nobody/widgets", "gitlab"); got != nil {
		t.Errorf("got %q for an unregistered prefix", got.Prefix)
	}
}

func TestForWithoutAConnection(t *testing.T) {
	// No connection is not an error: the upload lands and the forge
	// surfaces report that they were skipped.
	f, _ := newForges(t, nil)
	fg, err := f.For(t.Context(), &store.Repo{Forge: "bitbucket", Slug: "acme/widgets"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if fg != nil {
		t.Errorf("forge = %#v, want nil", fg)
	}
}
