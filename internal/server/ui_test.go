package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/store"
)

func doGet(t *testing.T, f *fixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestUncoveredRanges(t *testing.T) {
	b := func(start, end, stmts, count int) profile.Block {
		return profile.Block{StartLine: start, EndLine: end, NumStmts: stmts, Count: count}
	}
	tests := []struct {
		name   string
		blocks []profile.Block
		want   string
	}{
		{"fully covered", []profile.Block{b(1, 5, 3, 2)}, ""},
		{"single line", []profile.Block{b(7, 7, 1, 0)}, "7"},
		{"range", []profile.Block{b(10, 14, 3, 0)}, "10-14"},
		{"adjacent blocks merge", []profile.Block{b(10, 12, 2, 0), b(13, 15, 2, 0)}, "10-15"},
		{"overlapping blocks merge", []profile.Block{b(10, 14, 2, 0), b(12, 16, 2, 0)}, "10-16"},
		{"mixed with covered", []profile.Block{b(1, 5, 3, 9), b(8, 9, 2, 0), b(20, 20, 1, 0)}, "8-9, 20"},
		{"unsorted input", []profile.Block{b(20, 22, 1, 0), b(3, 4, 1, 0)}, "3-4, 20-22"},
		{"zero statement blocks ignored", []profile.Block{b(5, 6, 0, 0)}, ""},
		{
			"capped with more marker",
			[]profile.Block{b(1, 1, 1, 0), b(3, 3, 1, 0), b(5, 5, 1, 0), b(7, 7, 1, 0),
				b(9, 9, 1, 0), b(11, 11, 1, 0), b(13, 13, 1, 0), b(15, 15, 1, 0)},
			"1, 3, 5, 7, 9, 11, +2 more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uncoveredRanges(tt.blocks); got != tt.want {
				t.Errorf("uncoveredRanges() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIndexSearchFilter(t *testing.T) {
	f := newFixture(t, nil)
	second := &store.Repo{Forge: "bitbucket", Slug: "acme/gadgets", Token: "tok2", DefaultBranch: "main"}
	if err := f.store.CreateRepo(t.Context(), second); err != nil {
		t.Fatal(err)
	}

	body := doGet(t, f, "/?q=widg").Body.String()
	if !strings.Contains(body, "acme/widgets") || strings.Contains(body, "acme/gadgets") {
		t.Errorf("search filter failed: %s", body)
	}
	// Case-insensitive.
	body = doGet(t, f, "/?q=GADG").Body.String()
	if strings.Contains(body, "acme/widgets") || !strings.Contains(body, "acme/gadgets") {
		t.Errorf("case-insensitive search failed")
	}
}

func TestIndexShowsGateAndDelta(t *testing.T) {
	f := newFixture(t, nil)
	// 80% baseline first; the gate arrives afterwards so the baseline
	// remains usable for the delta.
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	f.repo.Gate = store.Gate{MinCoverage: pctPtr(90)}
	if err := f.store.UpdateRepo(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	better := "mode: set\nexample.com/m/a.go:1.1,5.2 10 3\n" // 100%, passes the gate
	doUpload(t, f, "secret-token", map[string]string{"commit": "c2", "branch": "main"}, better)

	body := doGet(t, f, "/").Body.String()
	if !strings.Contains(body, "chip pass") {
		t.Errorf("gate chip missing: %s", body)
	}
	if !strings.Contains(body, "delta up") || !strings.Contains(body, "20.0%") {
		t.Errorf("delta missing: %s", body)
	}
}

func TestIndexDeltaSkipsGateFailedBaselines(t *testing.T) {
	f := newFixture(t, nil)
	// 80% baseline before any gate exists.
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	f.repo.Gate = store.Gate{MinCoverage: pctPtr(90)}
	if err := f.store.UpdateRepo(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	// 50%: fails the gate, must never become a delta baseline.
	worse := "mode: set\nexample.com/m/a.go:1.1,5.2 1 1\nexample.com/m/a.go:6.1,7.2 1 0\n"
	doUpload(t, f, "secret-token", map[string]string{"commit": "c2", "branch": "main"}, worse)
	// 100%: passes. Delta must be vs c1 (80%) = +20, not vs c2 (50%) = +50.
	best := "mode: set\nexample.com/m/a.go:1.1,5.2 10 3\n"
	doUpload(t, f, "secret-token", map[string]string{"commit": "c3", "branch": "main"}, best)

	body := doGet(t, f, "/").Body.String()
	if !strings.Contains(body, "20.0%") || strings.Contains(body, "50.0%") {
		t.Errorf("delta must use the last gate-passing baseline: %s", body)
	}
}

func TestRepoBranchFilterAndPagination(t *testing.T) {
	f := newFixture(t, nil)
	doUpload(t, f, "secret-token", map[string]string{"commit": "main1", "branch": "main"}, testProfile)
	doUpload(t, f, "secret-token", map[string]string{"commit": "feat1", "branch": "feat"}, testProfile)

	body := doGet(t, f, "/repos/acme/widgets?branch=feat").Body.String()
	if !strings.Contains(body, "feat1") || strings.Contains(body, "main1") {
		t.Errorf("branch filter failed: %s", body)
	}
	// The branch selector lists both branches.
	if !strings.Contains(body, `value="feat" selected`) || !strings.Contains(body, `value="main"`) {
		t.Errorf("branch selector wrong: %s", body)
	}
	// The badge box carries a copy button wired to the markdown input.
	if !strings.Contains(body, `data-copy="#badge-md"`) || !strings.Contains(body, `id="badge-md"`) {
		t.Errorf("badge copy button missing: %s", body)
	}

	// 26 more uploads on main force pagination.
	for i := 0; i < 26; i++ {
		doUpload(t, f, "secret-token", map[string]string{
			"commit": "bulk" + strings.Repeat("x", i%3) + string(rune('a'+i%26)), "branch": "main",
		}, testProfile)
	}
	page0 := doGet(t, f, "/repos/acme/widgets?branch=main").Body.String()
	if !strings.Contains(page0, "Older") {
		t.Errorf("page 0 missing Older link")
	}
	page1 := doGet(t, f, "/repos/acme/widgets?branch=main&page=1").Body.String()
	if !strings.Contains(page1, "Newer") {
		t.Errorf("page 1 missing Newer link")
	}
	if !strings.Contains(page1, "main1") {
		t.Errorf("oldest upload not on the last page")
	}
}

func TestStaticAssetsServed(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{"/static/style.css", "/static/htmx.min.js", "/static/app.js", "/static/favicon.svg"} {
		rec := doGet(t, f, path)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: code=%d len=%d", path, rec.Code, rec.Body.Len())
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("%s: no cache header (%q)", path, cc)
		}
	}
}

func TestUploadPageShowsUncoveredRanges(t *testing.T) {
	f := newFixture(t, nil)
	// a.go block 7.1,9.2 is uncovered in testProfile -> "7-9".
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	body := doGet(t, f, "/uploads/1").Body.String()
	if !strings.Contains(body, `class="uncov"`) || !strings.Contains(body, "7-9") {
		t.Errorf("uncovered ranges missing: %s", body)
	}
}
