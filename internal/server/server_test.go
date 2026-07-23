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
		Store: st,
		Blobs: blobs,
		Parsers: map[string]profile.Parser{
			"go":        profile.GoParser{},
			"lcov":      profile.LCOVParser{},
			"jacoco":    profile.JaCoCoParser{},
			"cobertura": profile.CoberturaParser{},
		},
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

	t.Run("invalid token rejected before body parsing", func(t *testing.T) {
		// A garbage body with a bad token must yield 401, not 400: the
		// token check runs before the multipart parse.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", strings.NewReader("not multipart"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 before parsing", rec.Code)
		}
	})
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
		{"unknown format", map[string]string{"commit": "c", "format": "clover"}, testProfile, http.StatusBadRequest},
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

func TestUploadLCOV(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	// PR diff touches src/app.js lines 2 (covered) and 5 (uncovered).
	f.forge.DiffText = `diff --git a/src/app.js b/src/app.js
--- a/src/app.js
+++ b/src/app.js
@@ -1,5 +1,5 @@
 ctx
-old
+added 2
 ctx
 ctx
-old
+added 5
`
	lcov := `SF:src/app.js
DA:1,1
DA:2,1
DA:3,2
DA:5,0
end_of_record
SF:src/util.js
DA:1,0
end_of_record
`
	// No format field: the server must sniff LCOV from the content.
	rec := doUpload(t, f, "secret-token", map[string]string{
		"commit": "jsc1", "branch": "main", "pr_id": "9",
	}, lcov)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 3 of 5 lines covered across both files.
	if resp.TotalPct != 60 || resp.CoveredStmts != 3 || resp.TotalStmts != 5 {
		t.Errorf("totals = %.1f%% %d/%d, want 60%% 3/5", resp.TotalPct, resp.CoveredStmts, resp.TotalStmts)
	}
	// Diff coverage: line 2 covered, line 5 uncovered -> 1/2. Exact path
	// match, no path_prefix needed for repo-relative lcov paths.
	if resp.DiffStatus != "computed" || resp.DiffTotalLines == nil || *resp.DiffTotalLines != 2 ||
		*resp.DiffCoveredLines != 1 {
		t.Errorf("diff = %v/%v (%s), want 1/2 computed; body = %s",
			resp.DiffCoveredLines, resp.DiffTotalLines, resp.DiffStatus, rec.Body)
	}
	// The sniffed format is what gets stored.
	u, err := f.store.Upload(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Format != "lcov" {
		t.Errorf("stored format = %q, want lcov (sniffed)", u.Format)
	}
}

func TestUploadJaCoCo(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	// PR touches Foo.java under its source root: line 11 covered, 13 not.
	f.forge.DiffText = `diff --git a/src/main/java/com/example/app/Foo.java b/src/main/java/com/example/app/Foo.java
--- a/src/main/java/com/example/app/Foo.java
+++ b/src/main/java/com/example/app/Foo.java
@@ -10,4 +10,4 @@
 ctx
-old
+added 11
 ctx
-old
+added 13
`
	jacoco := `<?xml version="1.0" encoding="UTF-8"?>
<report name="app">
  <package name="com/example/app">
    <sourcefile name="Foo.java">
      <line nr="10" mi="0" ci="4"/>
      <line nr="11" mi="0" ci="4"/>
      <line nr="13" mi="2" ci="0"/>
    </sourcefile>
  </package>
</report>
`
	// No format field: sniffed from the XML content.
	rec := doUpload(t, f, "secret-token", map[string]string{
		"commit": "javac1", "branch": "main", "pr_id": "3",
	}, jacoco)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalPct != float64(2)/float64(3)*100 || resp.CoveredStmts != 2 || resp.TotalStmts != 3 {
		t.Errorf("totals = %v%% %d/%d, want 2/3", resp.TotalPct, resp.CoveredStmts, resp.TotalStmts)
	}
	// Reverse suffix matching bridges src/main/java: 1/2 changed lines.
	if resp.DiffStatus != "computed" || resp.DiffTotalLines == nil || *resp.DiffTotalLines != 2 ||
		*resp.DiffCoveredLines != 1 {
		t.Errorf("diff = %v/%v (%s), want 1/2 computed; body = %s",
			resp.DiffCoveredLines, resp.DiffTotalLines, resp.DiffStatus, rec.Body)
	}
	u, err := f.store.Upload(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Format != "jacoco" {
		t.Errorf("stored format = %q, want jacoco (sniffed)", u.Format)
	}
}

func TestUploadCobertura(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	// PR touches myapp/app.py: line 2 covered, line 6 not.
	f.forge.DiffText = `diff --git a/myapp/app.py b/myapp/app.py
--- a/myapp/app.py
+++ b/myapp/app.py
@@ -1,6 +1,6 @@
 ctx
-old
+added 2
 ctx
 ctx
 ctx
-old
+added 6
`
	cobertura := `<?xml version="1.0" ?>
<coverage lines-valid="4" lines-covered="3" line-rate="0.75">
  <packages><package name="myapp"><classes>
    <class name="app.py" filename="myapp/app.py">
      <lines>
        <line number="1" hits="1"/>
        <line number="2" hits="4"/>
        <line number="3" hits="4"/>
        <line number="6" hits="0"/>
      </lines>
    </class>
  </classes></package></packages>
</coverage>
`
	// No format field: sniffed from the XML content.
	rec := doUpload(t, f, "secret-token", map[string]string{
		"commit": "pyc1", "branch": "main", "pr_id": "5",
	}, cobertura)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalPct != 75 || resp.CoveredStmts != 3 || resp.TotalStmts != 4 {
		t.Errorf("totals = %v%% %d/%d, want 75%% 3/4", resp.TotalPct, resp.CoveredStmts, resp.TotalStmts)
	}
	// Repo-relative cobertura paths match the diff exactly: 1/2.
	if resp.DiffStatus != "computed" || resp.DiffTotalLines == nil || *resp.DiffTotalLines != 2 ||
		*resp.DiffCoveredLines != 1 {
		t.Errorf("diff = %v/%v (%s), want 1/2 computed; body = %s",
			resp.DiffCoveredLines, resp.DiffTotalLines, resp.DiffStatus, rec.Body)
	}
	u, err := f.store.Upload(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Format != "cobertura" {
		t.Errorf("stored format = %q, want cobertura (sniffed)", u.Format)
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

func TestWorkspaceTokenUpload(t *testing.T) {
	ctx := context.Background()
	newWsFixture := func(t *testing.T, wsDefaultBranch, forgeDefaultBranch string, withGlobalCreds bool) *fixture {
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
			Forges:  map[string]forge.Factory{"bitbucket": ff.Factory()},
			BaseURL: "https://gocov.example",
		}
		if withGlobalCreds {
			cfg.DefaultForgeCredentials = map[string]map[string]string{
				"bitbucket": {"username": "bot", "app_password": "botpass"},
			}
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
		repo, err := f.store.RepoBySlug(ctx, "acme/newrepo")
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
		// No global credentials: the forge cannot be asked.
		f := newWsFixture(t, "develop", "development", false)
		doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c1"}, testProfile)
		repo, err := f.store.RepoBySlug(ctx, "acme/newrepo")
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
		repo, err := f.store.RepoBySlug(ctx, "acme/newrepo")
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
		if _, err := f.store.RepoBySlug(ctx, "acme/ghost"); !errors.Is(err, store.ErrNotFound) {
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
		repo, err := f.store.RepoBySlug(ctx, "acme/newrepo")
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

func TestDefaultForgeCredentials(t *testing.T) {
	newSrvWith := func(t *testing.T, repoCreds map[string]string, factory forge.Factory) *Server {
		t.Helper()
		st := storemem.New()
		repo := &store.Repo{
			Forge: "bitbucket", Slug: "acme/widgets", Token: "secret-token",
			DefaultBranch: "main", ForgeCredentials: repoCreds,
		}
		if err := st.CreateRepo(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		return New(Config{
			Store:   st,
			Blobs:   blobmem.New(),
			Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
			Forges:  map[string]forge.Factory{"bitbucket": factory},
			DefaultForgeCredentials: map[string]map[string]string{
				"bitbucket": {"username": "bot", "app_password": "botpass"},
			},
		})
	}
	upload := func(t *testing.T, srv *Server) uploadResponse {
		t.Helper()
		body, ct := multipartUpload(t, map[string]string{"commit": "c"}, testProfile)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer secret-token")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body: %s", rec.Body)
		}
		return resp
	}

	t.Run("repo without credentials falls back to global bot", func(t *testing.T) {
		var gotCreds map[string]string
		ff := forgefake.New()
		factory := func(creds map[string]string) (forge.Forge, error) {
			gotCreds = creds
			return ff, nil
		}
		if resp := upload(t, newSrvWith(t, nil, factory)); resp.BuildStatus != "posted" {
			t.Errorf("build_status = %q, want posted via global credentials", resp.BuildStatus)
		}
		if gotCreds["username"] != "bot" {
			t.Errorf("factory got %v, want global bot credentials", gotCreds)
		}
	})

	t.Run("per-repo credentials take precedence", func(t *testing.T) {
		var gotCreds map[string]string
		ff := forgefake.New()
		factory := func(creds map[string]string) (forge.Forge, error) {
			gotCreds = creds
			return ff, nil
		}
		own := map[string]string{"username": "own", "app_password": "ownpass"}
		if resp := upload(t, newSrvWith(t, own, factory)); resp.BuildStatus != "posted" {
			t.Errorf("build_status = %q", resp.BuildStatus)
		}
		if gotCreds["username"] != "own" {
			t.Errorf("factory got %v, want per-repo credentials", gotCreds)
		}
	})
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
