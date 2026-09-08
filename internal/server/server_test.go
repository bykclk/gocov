package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
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

const testProfile = `mode: set
example.com/m/a.go:1.1,5.2 6 1
example.com/m/a.go:7.1,9.2 2 0
example.com/m/b.go:1.1,3.2 2 1
`

// testProfile: a.go 6/8, b.go 2/2, total 8/10 = 80%.

type fixture struct {
	srv   *Server
	store *storemem.Store
	blobs *blobmem.Store
	forge *forgefake.Forge
	repo  *store.Repo
}

// newFixture builds a bitbucket-forge server with the repo acme/widgets.
// A non-nil connected map wires a one-click Bitbucket connection (grant
// on workspace acme) so the repo's forge surfaces are reachable through
// f.forge; nil leaves the repo with no forge access. The map's contents
// are ignored — only nil vs non-nil matters — so existing call sites that
// passed credential maps keep working.

func newFixture(t *testing.T, connected map[string]string) *fixture {
	t.Helper()
	ctx := t.Context()
	st := storemem.New()
	repo := &store.Repo{
		Forge:         "bitbucket",
		Slug:          "acme/widgets",
		Token:         "secret-token",
		DefaultBranch: "main",
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	ff := forgefake.New()
	cfg := Config{
		Store: st,
		Blobs: blobs,
		Parsers: map[string]profile.Parser{
			"go":        profile.GoParser{},
			"lcov":      profile.LCOVParser{},
			"jacoco":    profile.JaCoCoParser{},
			"cobertura": profile.CoberturaParser{},
			"clover":    profile.CloverParser{},
			"simplecov": profile.SimpleCovParser{},
		},
		BaseURL: "https://gocov.example",
	}
	if connected != nil {
		ws := &store.Workspace{Forge: "bitbucket", Prefix: "acme", Token: "ws-secret", DefaultBranch: "main"}
		if err := st.CreateWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if err := st.SetWorkspaceBitbucketGrant(ctx, ws.ID, "covbot", "rt-0", false); err != nil {
			t.Fatal(err)
		}
		cfg.BitbucketConnect = &fakeBBConnect{grantForge: ff}
	}
	return &fixture{srv: New(cfg), store: st, blobs: blobs, forge: ff, repo: repo}
}

func multipartUpload(t *testing.T, fields map[string]string, profileBody string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if profileBody != "" {
		fw, err := mw.CreateFormFile("profile", "coverage.out")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, profileBody); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func doUpload(t *testing.T, f *fixture, token string, fields map[string]string, profileBody string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartUpload(t, fields, profileBody)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func getAccept(f *fixture, path, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	get := func(srv *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec
	}
	base := Config{
		Store:   storemem.New(),
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
	}

	t.Run("no probe configured", func(t *testing.T) {
		if rec := get(New(base)); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("healthy probe", func(t *testing.T) {
		cfg := base
		cfg.Health = func(context.Context) error { return nil }
		if rec := get(New(cfg)); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("failing probe", func(t *testing.T) {
		cfg := base
		cfg.Health = func(context.Context) error { return errFake }
		if rec := get(New(cfg)); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})
}

func TestPages(t *testing.T) {
	f := newFixture(t, nil)
	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "abc123def456789", "branch": "main"}, testProfile)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}

	// Index lists the repo with its coverage.
	idx := get("/")
	if idx.Code != http.StatusOK || !strings.Contains(idx.Body.String(), "acme/widgets") {
		t.Errorf("index: code=%d body=%s", idx.Code, idx.Body)
	}
	if !strings.Contains(idx.Body.String(), "80.0%") {
		t.Errorf("index does not show coverage")
	}

	// Repo page lists the upload.
	repoPage := get("/repos/bitbucket/acme/widgets")
	if repoPage.Code != http.StatusOK || !strings.Contains(repoPage.Body.String(), "abc123def456") {
		t.Errorf("repo page: code=%d", repoPage.Code)
	}

	// Upload page shows per-file rows.
	upPage := get("/uploads/1")
	body := upPage.Body.String()
	if upPage.Code != http.StatusOK ||
		!strings.Contains(body, "example.com/m/a.go") ||
		!strings.Contains(body, "example.com/m/b.go") {
		t.Errorf("upload page: code=%d body=%s", upPage.Code, body)
	}

	if rec := get("/repos/bitbucket/no/such"); rec.Code != http.StatusNotFound {
		t.Errorf("missing repo page: code=%d, want 404", rec.Code)
	}
	if rec := get("/uploads/999"); rec.Code != http.StatusNotFound {
		t.Errorf("missing upload page: code=%d, want 404", rec.Code)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{"/static/style.css", "/static/htmx.min.js", "/static/app.js", "/static/favicon.svg"} {
		rec := get(f, path)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: code=%d len=%d", path, rec.Code, rec.Body.Len())
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("%s: no cache header (%q)", path, cc)
		}
	}
}

func TestNotFoundPage(t *testing.T) {
	f := newFixture(t, nil)

	t.Run("catch-all renders styled page for browser GET", func(t *testing.T) {
		rec := getAccept(f, "/does/not/exist", "text/html")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "404 · not found") {
			t.Errorf("body missing 404 badge:\n%s", body)
		}
		if !strings.Contains(body, "/does/not/exist") {
			t.Errorf("body missing requested path:\n%s", body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("content-type = %q, want text/html", ct)
		}
	})

	t.Run("catch-all stays plain for non-browser GET", func(t *testing.T) {
		rec := getAccept(f, "/does/not/exist", "application/json")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "404 · not found") {
			t.Error("plain 404 should not carry the styled page")
		}
	})

	t.Run("missing repo renders styled page", func(t *testing.T) {
		rec := get(f, "/repos/bitbucket/acme/ghost")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "404 · not found") {
			t.Errorf("missing repo did not render styled 404:\n%s", rec.Body.String())
		}
	})

	t.Run("missing upload renders styled page", func(t *testing.T) {
		rec := get(f, "/uploads/9999")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "404 · not found") {
			t.Errorf("missing upload did not render styled 404:\n%s", rec.Body.String())
		}
	})
}
