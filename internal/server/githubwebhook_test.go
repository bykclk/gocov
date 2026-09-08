package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	forgefake "github.com/gocov/gocov/internal/forge/fake"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

const webhookSecret = "shhh"

// webhookServer builds a server with the webhook enabled and one github
// workspace linked to installation 4242, connected through the App to
// the returned fake forge (the client repository events verify against).
func webhookServer(t *testing.T) (*Server, *storemem.Store, *forgefake.Forge) {
	t.Helper()
	st := storemem.New()
	if err := st.CreateWorkspace(t.Context(), &store.Workspace{
		Forge: "github", Prefix: "acme", Token: "tok", DefaultBranch: "main",
		GitHubInstallationID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	ff := forgefake.New()
	srv := New(Config{
		Store:               st,
		Blobs:               blobmem.New(),
		Parsers:             map[string]profile.Parser{"go": profile.GoParser{}},
		GitHubApp:           &fakeGitHubApp{appForge: ff},
		GitHubWebhookSecret: webhookSecret,
	})
	return srv, st, ff
}

func postWebhook(srv *Server, event, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	srv, _, _ := webhookServer(t)
	body := `{"action":"purchased"}`

	if rec := postWebhook(srv, "marketplace_purchase", body, "sha256=deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong signature: status = %d, want 401", rec.Code)
	}
	if rec := postWebhook(srv, "marketplace_purchase", body, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing signature: status = %d, want 401", rec.Code)
	}
	// A signature over different bytes must not verify.
	if rec := postWebhook(srv, "marketplace_purchase", body, sign(webhookSecret, "tampered")); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered body: status = %d, want 401", rec.Code)
	}
}

func TestWebhookMarketplacePurchase(t *testing.T) {
	srv, _, _ := webhookServer(t)
	body := `{"action":"purchased","marketplace_purchase":{"account":{"login":"acme","type":"Organization"},"plan":{"name":"Free"}}}`
	rec := postWebhook(srv, "marketplace_purchase", body, sign(webhookSecret, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("marketplace_purchase: status = %d, want 200", rec.Code)
	}
}

func TestWebhookPingAndUnknownEvent(t *testing.T) {
	srv, _, _ := webhookServer(t)
	for _, event := range []string{"ping", "push"} {
		body := `{"zen":"go"}`
		if rec := postWebhook(srv, event, body, sign(webhookSecret, body)); rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", event, rec.Code)
		}
	}
}

func TestWebhookInstallationFlipsBrokenFlag(t *testing.T) {
	srv, st, _ := webhookServer(t)

	broken := func() bool {
		ws, err := st.WorkspaceByPrefix(t.Context(), "github", "acme")
		if err != nil {
			t.Fatal(err)
		}
		return ws.GitHubAppBroken
	}

	del := `{"action":"deleted","installation":{"id":4242}}`
	if rec := postWebhook(srv, "installation", del, sign(webhookSecret, del)); rec.Code != http.StatusOK {
		t.Fatalf("installation deleted: status = %d", rec.Code)
	}
	if !broken() {
		t.Error("installation deleted did not mark the workspace broken")
	}

	heal := `{"action":"unsuspend","installation":{"id":4242}}`
	if rec := postWebhook(srv, "installation", heal, sign(webhookSecret, heal)); rec.Code != http.StatusOK {
		t.Fatalf("installation unsuspend: status = %d", rec.Code)
	}
	if broken() {
		t.Error("installation unsuspend did not heal the workspace")
	}

	// An event for an unrelated installation must not touch our workspace.
	other := `{"action":"deleted","installation":{"id":9999}}`
	if rec := postWebhook(srv, "installation", other, sign(webhookSecret, other)); rec.Code != http.StatusOK {
		t.Fatalf("unrelated installation: status = %d", rec.Code)
	}
	if broken() {
		t.Error("unrelated installation event flipped the flag")
	}
}

func TestWebhookRepositoryVisibilityChange(t *testing.T) {
	srv, st, ff := webhookServer(t)
	ctx := t.Context()
	repo := &store.Repo{
		Forge: "github", Slug: "acme/widgets", Token: "tok-r",
		DefaultBranch: "main", Visibility: store.VisibilityPublic,
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}

	visibility := func(forge, slug string) *store.Repo {
		t.Helper()
		r, err := st.RepoBySlug(ctx, forge, slug)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// privatized closes the cached answer the moment GitHub says so — no
	// forge round-trip, the fail-closed direction is trusted as-is.
	priv := `{"action":"privatized","repository":{"full_name":"acme/widgets"}}`
	if rec := postWebhook(srv, "repository", priv, sign(webhookSecret, priv)); rec.Code != http.StatusOK {
		t.Fatalf("repository privatized: status = %d", rec.Code)
	}
	if got := visibility("github", "acme/widgets"); got.Visibility != store.VisibilityPrivate {
		t.Errorf("visibility after privatized = %q, want private", got.Visibility)
	} else if got.VisibilityCheckedAt.IsZero() {
		t.Error("webhook flip did not stamp VisibilityCheckedAt")
	}
	if len(ff.VisibilityCalls) != 0 {
		t.Errorf("privatized asked the forge (%d calls); the closing direction needs no verification", len(ff.VisibilityCalls))
	}

	// A stale (redelivered, out-of-order) publicized must not reopen the
	// pages on its own say-so: it triggers a re-verification through the
	// connection, and the forge still answers private.
	ff.Visibility = store.VisibilityPrivate
	pub := `{"action":"publicized","repository":{"full_name":"acme/widgets"}}`
	if rec := postWebhook(srv, "repository", pub, sign(webhookSecret, pub)); rec.Code != http.StatusOK {
		t.Fatalf("repository publicized: status = %d", rec.Code)
	}
	if got := visibility("github", "acme/widgets"); got.Visibility != store.VisibilityPrivate {
		t.Errorf("a publicized event the forge contradicts reopened the repo: %q", got.Visibility)
	}
	if len(ff.VisibilityCalls) != 1 {
		t.Errorf("publicized visibility calls = %d, want 1 (verified through the connection)", len(ff.VisibilityCalls))
	}

	// A truthful publicized reopens it — verified, not trusted.
	ff.Visibility = store.VisibilityPublic
	if rec := postWebhook(srv, "repository", pub, sign(webhookSecret, pub)); rec.Code != http.StatusOK {
		t.Fatalf("repository publicized again: status = %d", rec.Code)
	}
	if got := visibility("github", "acme/widgets"); got.Visibility != store.VisibilityPublic {
		t.Errorf("visibility after verified publicized = %q, want public", got.Visibility)
	}

	// An untracked repo is a no-op, still acknowledged with 2xx.
	ghost := `{"action":"privatized","repository":{"full_name":"acme/ghost"}}`
	if rec := postWebhook(srv, "repository", ghost, sign(webhookSecret, ghost)); rec.Code != http.StatusOK {
		t.Errorf("untracked repo: status = %d", rec.Code)
	}

	// A same-named repo tracked on another forge must not be flipped by a
	// GitHub event.
	bb := &store.Repo{
		Forge: "bitbucket", Slug: "beta/things", Token: "tok-bb",
		DefaultBranch: "main", Visibility: store.VisibilityPublic,
	}
	if err := st.CreateRepo(ctx, bb); err != nil {
		t.Fatal(err)
	}
	cross := `{"action":"privatized","repository":{"full_name":"beta/things"}}`
	if rec := postWebhook(srv, "repository", cross, sign(webhookSecret, cross)); rec.Code != http.StatusOK {
		t.Fatalf("cross-forge slug: status = %d", rec.Code)
	}
	if got := visibility("bitbucket", "beta/things"); got.Visibility != store.VisibilityPublic {
		t.Errorf("a GitHub event flipped a bitbucket repo to %q", got.Visibility)
	}

	// Other repository actions are ignored.
	ren := `{"action":"renamed","repository":{"full_name":"acme/widgets"}}`
	if rec := postWebhook(srv, "repository", ren, sign(webhookSecret, ren)); rec.Code != http.StatusOK {
		t.Errorf("renamed: status = %d", rec.Code)
	}
	if got := visibility("github", "acme/widgets"); got.Visibility != store.VisibilityPublic {
		t.Errorf("renamed changed visibility to %q", got.Visibility)
	}
}

func TestWebhookRouteAbsentWithoutSecret(t *testing.T) {
	st := storemem.New()
	srv := New(Config{
		Store:   st,
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		// No GitHubWebhookSecret.
	})
	body := `{"action":"purchased"}`
	if rec := postWebhook(srv, "marketplace_purchase", body, sign(webhookSecret, body)); rec.Code != http.StatusNotFound {
		t.Errorf("webhook without secret: status = %d, want 404", rec.Code)
	}
}
