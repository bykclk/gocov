package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/forge"
	forgefake "github.com/bykclk/gocov/internal/forge/fake"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
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

func newFixture(t *testing.T, creds map[string]string) *fixture {
	t.Helper()
	st := storemem.New()
	repo := &store.Repo{
		Forge:            "bitbucket",
		Slug:             "acme/widgets",
		Token:            "secret-token",
		DefaultBranch:    "main",
		ForgeCredentials: creds,
	}
	if err := st.CreateRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	blobs := blobmem.New()
	ff := forgefake.New()
	srv := New(Config{
		Store:   st,
		Blobs:   blobs,
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		Forges:  map[string]forge.Factory{"bitbucket": ff.Factory()},
		BaseURL: "https://gocov.example",
	})
	return &fixture{srv: srv, store: st, blobs: blobs, forge: ff, repo: repo}
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

func TestUploadHappyPath(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	rec := doUpload(t, f, "secret-token", map[string]string{
		"repo":   "acme/widgets",
		"commit": "abc123def456",
		"branch": "main",
	}, testProfile)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalPct != 80 || resp.CoveredStmts != 8 || resp.TotalStmts != 10 {
		t.Errorf("totals = %.1f%% %d/%d, want 80%% 8/10", resp.TotalPct, resp.CoveredStmts, resp.TotalStmts)
	}
	if resp.DeltaPct != nil {
		t.Errorf("first upload should have no delta, got %v", *resp.DeltaPct)
	}
	if resp.BuildStatus != "posted" {
		t.Errorf("build_status = %q, want posted", resp.BuildStatus)
	}

	// Stored upload and per-file rows.
	u, err := f.store.Upload(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.CommitSHA != "abc123def456" || u.Branch != "main" || u.Format != "go" {
		t.Errorf("stored upload = %+v", u)
	}
	files, err := f.store.UploadFiles(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0].Path != "example.com/m/a.go" || files[0].Pct != 75 {
		t.Errorf("file[0] = %s %.1f%%, want a.go 75%%", files[0].Path, files[0].Pct)
	}
	if len(files[0].Blocks) != 2 {
		t.Errorf("file[0] blocks = %d, want 2 (block data must be preserved)", len(files[0].Blocks))
	}

	// Raw profile persisted in the blobstore.
	raw, err := f.blobs.Get(context.Background(), u.RawBlobKey)
	if err != nil {
		t.Fatalf("raw blob: %v", err)
	}
	if string(raw) != testProfile {
		t.Error("raw blob does not match uploaded profile")
	}

	// Build status pushed to the forge.
	if len(f.forge.StatusCalls) != 1 {
		t.Fatalf("got %d status calls, want 1", len(f.forge.StatusCalls))
	}
	call := f.forge.StatusCalls[0]
	if call.RepoSlug != "acme/widgets" || call.CommitSHA != "abc123def456" {
		t.Errorf("status call = %+v", call)
	}
	if call.Status.Description != "coverage: 80.0%" {
		t.Errorf("description = %q, want %q", call.Status.Description, "coverage: 80.0%")
	}
	if !strings.HasPrefix(call.Status.URL, "https://gocov.example/uploads/") {
		t.Errorf("status URL = %q", call.Status.URL)
	}
}

func TestUploadDelta(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	// First upload: 80%.
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1"}, testProfile)

	// Second upload: 100%.
	better := "mode: set\nexample.com/m/a.go:1.1,5.2 10 3\n"
	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c2"}, better)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DeltaPct == nil || *resp.DeltaPct != 20 {
		t.Fatalf("delta = %v, want +20", resp.DeltaPct)
	}
	last := f.forge.StatusCalls[len(f.forge.StatusCalls)-1]
	if last.Status.Description != "coverage: 100.0% (+20.0%)" {
		t.Errorf("description = %q", last.Status.Description)
	}
}

func TestUploadFeatureBranchDeltaAgainstDefault(t *testing.T) {
	f := newFixture(t, nil)
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)

	worse := "mode: set\nexample.com/m/a.go:1.1,5.2 2 1\nexample.com/m/a.go:6.1,7.2 2 0\n"
	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c2", "branch": "feature/x"}, worse)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DeltaPct == nil || *resp.DeltaPct != -30 {
		t.Fatalf("delta = %v, want -30 (50%% vs 80%% on main)", resp.DeltaPct)
	}
}

func TestUploadAuth(t *testing.T) {
	f := newFixture(t, nil)
	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doUpload(t, f, tt.token, map[string]string{"commit": "c"}, testProfile)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestUploadValidation(t *testing.T) {
	f := newFixture(t, nil)
	tests := []struct {
		name    string
		fields  map[string]string
		profile string
		want    int
	}{
		{"repo mismatch", map[string]string{"repo": "other/repo", "commit": "c"}, testProfile, http.StatusForbidden},
		{"missing commit", map[string]string{}, testProfile, http.StatusBadRequest},
		{"missing profile file", map[string]string{"commit": "c"}, "", http.StatusBadRequest},
		{"unknown format", map[string]string{"commit": "c", "format": "lcov"}, testProfile, http.StatusBadRequest},
		{"malformed profile", map[string]string{"commit": "c"}, "not a profile", http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doUpload(t, f, "secret-token", tt.fields, tt.profile)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestUploadWithoutForgeCredentialsSkipsStatus(t *testing.T) {
	f := newFixture(t, nil)
	rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c"}, testProfile)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.BuildStatus != "skipped" {
		t.Errorf("build_status = %q, want skipped", resp.BuildStatus)
	}
	if len(f.forge.StatusCalls) != 0 {
		t.Errorf("forge was called despite missing credentials")
	}
}

// testPRDiff touches a.go: adds covered lines 2-3 (block 1.1,5.2 count 1),
// uncovered line 8 (block 7.1,9.2 count 0), non-executable line 20,
// plus an unmatched Go file and a doc file.
const testPRDiff = `diff --git a/m/a.go b/m/a.go
--- a/m/a.go
+++ b/m/a.go
@@ -1,3 +1,5 @@
 ctx
+added 2
+added 3
 ctx
 ctx
@@ -7,2 +8,3 @@
 ctx
+added line 9
 ctx
diff --git a/m/untested.go b/m/untested.go
--- /dev/null
+++ b/m/untested.go
@@ -0,0 +1,2 @@
+l1
+l2
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1,2 @@
 x
+docs
`

func TestUploadDiffCoverage(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	f.forge.DiffText = testPRDiff

	rec := doUpload(t, f, "secret-token", map[string]string{
		"commit": "prcommit1", "branch": "feature/x", "pr_id": "42",
	}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.DiffStatus != "computed" {
		t.Fatalf("diff_status = %q, body = %s", resp.DiffStatus, rec.Body)
	}
	// a.go: lines 2,3 covered; line 9 ("added 8 wait no" lands on line 9,
	// inside the uncovered 7-9 block). untested.go has no profile entry.
	if resp.DiffTotalLines == nil || *resp.DiffTotalLines != 3 ||
		resp.DiffCoveredLines == nil || *resp.DiffCoveredLines != 2 {
		t.Fatalf("diff lines = %v/%v, want 2/3; body = %s",
			resp.DiffCoveredLines, resp.DiffTotalLines, rec.Body)
	}
	if resp.PRComment != "posted" {
		t.Errorf("pr_comment = %q", resp.PRComment)
	}

	// The diff was requested for the right PR.
	if len(f.forge.DiffCalls) != 1 || f.forge.DiffCalls[0].PRID != "42" {
		t.Errorf("diff calls = %+v", f.forge.DiffCalls)
	}

	// Stored upload round-trips the result.
	u, err := f.store.Upload(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.DiffCoverage == nil || u.DiffCoverage.TotalLines != 3 {
		t.Fatalf("stored diff coverage = %+v", u.DiffCoverage)
	}
	if len(u.DiffCoverage.UnmatchedFiles) != 1 || u.DiffCoverage.UnmatchedFiles[0] != "m/untested.go" {
		t.Errorf("unmatched = %v, want [m/untested.go] (README.md filtered out)",
			u.DiffCoverage.UnmatchedFiles)
	}

	// PR comment content.
	if len(f.forge.CommentCalls) != 1 {
		t.Fatalf("comment calls = %d, want 1", len(f.forge.CommentCalls))
	}
	body := f.forge.CommentCalls[0].Body
	for _, want := range []string{
		"66.7%",         // diff pct 2/3
		"2/3",           // covered/total
		"m/a.go",        // uncovered file listed
		"m/untested.go", // unmatched file listed
		"/uploads/",     // report link
		"80.0%",         // total coverage
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if f.forge.CommentCalls[0].PRID != "42" {
		t.Errorf("comment PR = %q", f.forge.CommentCalls[0].PRID)
	}
}

func TestUploadDiffCoverageErrorPaths(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		f := newFixture(t, nil)
		rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c", "pr_id": "1"}, testProfile)
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusCreated || !strings.HasPrefix(resp.DiffStatus, "skipped") {
			t.Errorf("code=%d diff_status=%q", rec.Code, resp.DiffStatus)
		}
		if resp.PRComment != "skipped" {
			t.Errorf("pr_comment = %q", resp.PRComment)
		}
	})

	t.Run("forge diff not implemented", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		// fake forge with empty DiffText returns ErrNotImplemented
		rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c", "pr_id": "1"}, testProfile)
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusCreated || resp.DiffStatus != "skipped: diff not supported by forge" {
			t.Errorf("code=%d diff_status=%q", rec.Code, resp.DiffStatus)
		}
		// Comment still posted with total coverage only.
		if resp.PRComment != "posted" {
			t.Errorf("pr_comment = %q", resp.PRComment)
		}
	})

	t.Run("diff fetch error does not fail upload", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		f.forge.DiffErr = errFake
		rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c", "pr_id": "1"}, testProfile)
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusCreated || !strings.HasPrefix(resp.DiffStatus, "error:") {
			t.Errorf("code=%d diff_status=%q", rec.Code, resp.DiffStatus)
		}
	})

	t.Run("non-PR upload has no diff fields", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		f.forge.DiffText = testPRDiff
		rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c"}, testProfile)
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.DiffStatus != "" || resp.DiffPct != nil || resp.PRComment != "" {
			t.Errorf("non-PR upload leaked diff fields: %s", rec.Body)
		}
		if len(f.forge.DiffCalls) != 0 {
			t.Errorf("diff fetched for non-PR upload")
		}
	})
}

var errFake = errors.New("fake forge failure")

func TestBadge(t *testing.T) {
	tests := []struct {
		name      string
		profile   string // uploaded first when non-empty
		wantValue string
		wantColor string
	}{
		{"no uploads", "", "unknown", badgeGray},
		{"low red", "mode: set\na.go:1.1,2.2 6 0\na.go:3.1,4.2 4 1\n", "40.0%", badgeRed},
		{"mid yellow", "mode: set\na.go:1.1,2.2 5 0\na.go:3.1,4.2 5 1\n", "50.0%", badgeYellow},
		{"high boundary yellow", "mode: set\na.go:1.1,2.2 1 0\na.go:3.1,4.2 3 1\n", "75.0%", badgeYellow},
		{"green", "mode: set\na.go:1.1,2.2 1 0\na.go:3.1,4.2 9 1\n", "90.0%", badgeGreen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if tt.profile != "" {
				rec := doUpload(t, f, "secret-token", map[string]string{"commit": "c", "branch": "main"}, tt.profile)
				if rec.Code != http.StatusCreated {
					t.Fatalf("upload failed: %d %s", rec.Code, rec.Body)
				}
			}
			req := httptest.NewRequest(http.MethodGet, "/badge/acme/widgets.svg", nil)
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
				t.Errorf("content-type = %q", ct)
			}
			svg := rec.Body.String()
			if !strings.Contains(svg, ">"+tt.wantValue+"<") {
				t.Errorf("badge does not show %q: %s", tt.wantValue, svg)
			}
			if !strings.Contains(svg, tt.wantColor) {
				t.Errorf("badge does not use color %q", tt.wantColor)
			}
		})
	}
}

func TestBadgeUnknownRepo(t *testing.T) {
	f := newFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/badge/no/such.svg", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestBadgeUsesDefaultBranchOnly(t *testing.T) {
	f := newFixture(t, nil)
	// Only a feature-branch upload exists; the badge must stay "unknown".
	doUpload(t, f, "secret-token", map[string]string{"commit": "c", "branch": "feature/x"}, testProfile)
	req := httptest.NewRequest(http.MethodGet, "/badge/acme/widgets.svg", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), ">unknown<") {
		t.Errorf("badge should be unknown without default-branch uploads: %s", rec.Body)
	}
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
	repoPage := get("/repos/acme/widgets")
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

	if rec := get("/repos/no/such"); rec.Code != http.StatusNotFound {
		t.Errorf("missing repo page: code=%d, want 404", rec.Code)
	}
	if rec := get("/uploads/999"); rec.Code != http.StatusNotFound {
		t.Errorf("missing upload page: code=%d, want 404", rec.Code)
	}
}
