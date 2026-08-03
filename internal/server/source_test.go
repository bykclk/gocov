package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/profile"
)

// aGoSource has 9 lines; testProfile marks lines 1-5 covered (count 1)
// and 7-9 uncovered, line 6 is not executable.
const aGoSource = `package m

func covered() int {
	x := 1
	return x
}
func uncovered() int {
	return 2
}
`

func sourceFixture(t *testing.T) (*fixture, int64) {
	t.Helper()
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	f.forge.Files = map[string]string{"m/a.go": aGoSource}
	rec := doUpload(t, f, "secret-token", map[string]string{
		"commit": "c1", "branch": "main", "path_prefix": "example.com",
	}, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload failed: %d %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return f, resp.ID
}

func TestSourceView(t *testing.T) {
	f, id := sourceFixture(t)
	body := doGet(t, f, "/uploads/1/files/example.com/m/a.go").Body.String()

	// Covered lines render as hits with counts, uncovered as misses,
	// non-executable line 6 as neither.
	if !strings.Contains(body, `codeline hit`) || !strings.Contains(body, "1×") {
		t.Errorf("covered lines missing: %s", body)
	}
	if !strings.Contains(body, `codeline miss`) {
		t.Errorf("uncovered lines missing: %s", body)
	}
	if got := strings.Count(body, "codeline hit"); got != 5 {
		t.Errorf("hit lines = %d, want 5", got)
	}
	if got := strings.Count(body, "codeline miss"); got != 3 {
		t.Errorf("miss lines = %d, want 3", got)
	}
	// Source text is present and escaped by the template engine.
	if !strings.Contains(body, "func covered() int {") {
		t.Errorf("source text missing: %s", body)
	}
	_ = id
}

func TestSourceViewCachesContent(t *testing.T) {
	f, _ := sourceFixture(t)
	doGet(t, f, "/uploads/1/files/example.com/m/a.go")
	doGet(t, f, "/uploads/1/files/example.com/m/a.go")
	if got := len(f.forge.FileCalls); got != 1 {
		t.Errorf("forge fetched %d times, want 1 (cache)", got)
	}
	// The cache key uses the repo-relative path at the commit.
	if _, err := f.blobs.Get(context.Background(), "source/1/c1/m/a.go"); err != nil {
		t.Errorf("source not cached: %v", err)
	}
}

func TestSourceViewFallbacks(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		f := newFixture(t, nil)
		doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
		body := doGet(t, f, "/uploads/1/files/example.com/m/a.go").Body.String()
		if !strings.Contains(body, "Source is unavailable") {
			t.Errorf("fallback missing: %s", body)
		}
		// The uncovered summary still helps: block 7.1,9.2 is uncovered.
		if !strings.Contains(body, "7-9") {
			t.Errorf("uncovered ranges missing in fallback: %s", body)
		}
	})

	t.Run("file not on forge", func(t *testing.T) {
		f, _ := sourceFixture(t)
		body := doGet(t, f, "/uploads/1/files/example.com/m/b.go").Body.String()
		if !strings.Contains(body, "Source is unavailable") || !strings.Contains(body, "not found") {
			t.Errorf("not-found fallback missing: %s", body)
		}
	})

	t.Run("non-utf8 content", func(t *testing.T) {
		f, _ := sourceFixture(t)
		f.forge.Files["m/a.go"] = string([]byte{0xff, 0xfe, 0x00, 0x01})
		body := doGet(t, f, "/uploads/1/files/example.com/m/a.go").Body.String()
		if !strings.Contains(body, "not valid UTF-8") {
			t.Errorf("utf8 fallback missing: %s", body)
		}
	})

	t.Run("unknown paths and uploads 404", func(t *testing.T) {
		f, _ := sourceFixture(t)
		// Dot segments are redirected away by the mux's path cleaning and
		// the cleaned URL matches no route; the handler itself only serves
		// paths recorded in the upload, so nothing is ever exposed.
		rec := doGet(t, f, "/uploads/1/files/../../etc/passwd")
		if rec.Code == http.StatusOK {
			t.Errorf("traversal path must not be served: %d", rec.Code)
		}
		if loc := rec.Header().Get("Location"); strings.Contains(loc, "files") {
			t.Errorf("traversal redirect still points at the source view: %q", loc)
		}
		if rec := doGet(t, f, "/uploads/1/files/unknown.go"); rec.Code != http.StatusNotFound {
			t.Errorf("unknown file = %d, want 404", rec.Code)
		}
		if rec := doGet(t, f, "/uploads/99/files/example.com/m/a.go"); rec.Code != http.StatusNotFound {
			t.Errorf("unknown upload = %d, want 404", rec.Code)
		}
	})
}

func TestSourceViewSecurity(t *testing.T) {
	t.Run("dot segments in recorded paths never reach the forge", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		// A malicious profile records a path that would normalize into a
		// different forge API endpoint.
		evil := "mode: set\nexample.com/../../../user.go:1.1,2.2 1 1\n"
		rec := doUpload(t, f, "secret-token", map[string]string{
			"commit": "c1", "branch": "main", "path_prefix": "example.com",
		}, evil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload: %d %s", rec.Code, rec.Body)
		}
		body := doGet(t, f, "/uploads/1/files/example.com/../../../user.go")
		// Either the mux redirects the cleaned URL away, or the handler
		// refuses to ask the forge for it — the forge must never see it.
		if len(f.forge.FileCalls) != 0 {
			t.Errorf("forge was asked for %v", f.forge.FileCalls)
		}
		_ = body
	})

	t.Run("forge failure detail stays out of the page", func(t *testing.T) {
		f, _ := sourceFixture(t)
		f.forge.FileErr = errFake // "fake forge failure"
		body := doGet(t, f, "/uploads/1/files/example.com/m/a.go").Body.String()
		if strings.Contains(body, "fake forge failure") {
			t.Errorf("forge error text leaked into the page: %s", body)
		}
		if !strings.Contains(body, "fetching the file from the forge failed") {
			t.Errorf("generic reason missing: %s", body)
		}
	})

	t.Run("commit identifiers with separators are rejected at upload", func(t *testing.T) {
		f := newFixture(t, nil)
		for _, commit := range []string{"a/b", "a b", strings.Repeat("x", 65), "sha\n1"} {
			rec := doUpload(t, f, "secret-token", map[string]string{"commit": commit}, testProfile)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("commit %q: status = %d, want 400", commit, rec.Code)
			}
		}
	})
}

func TestRenderSourceLines(t *testing.T) {
	blocks := []profile.Block{
		{StartLine: 1, EndLine: 2, NumStmts: 1, Count: 3},
		{StartLine: 4, EndLine: 4, NumStmts: 1, Count: 0},
	}
	lines := renderSourceLines([]byte("a\nb\nc\nd\n"), blocks)
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4", len(lines))
	}
	if lines[0].Class != "hit" || lines[0].Hits != "3×" {
		t.Errorf("line 1 = %+v", lines[0])
	}
	if lines[2].Class != "" || lines[2].Hits != "" {
		t.Errorf("line 3 must be neutral: %+v", lines[2])
	}
	if lines[3].Class != "miss" {
		t.Errorf("line 4 = %+v", lines[3])
	}
	// Blocks beyond EOF must not panic.
	_ = renderSourceLines([]byte("only\n"), []profile.Block{{StartLine: 5, EndLine: 9, NumStmts: 1, Count: 1}})

	// Pre-validation rows with absurd ranges must not spin; this returns
	// promptly because the loop is clamped to the file length.
	_ = renderSourceLines([]byte("a\nb\n"), []profile.Block{{StartLine: -2_000_000_000, EndLine: 2_000_000_000, NumStmts: 1, Count: 1}})

	// CRLF sources render without the trailing carriage return.
	crlf := renderSourceLines([]byte("x\r\ny\r\n"), nil)
	if crlf[0].Text != "x" || crlf[1].Text != "y" {
		t.Errorf("crlf lines = %+v", crlf)
	}
}
