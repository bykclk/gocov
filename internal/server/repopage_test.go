package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gocov/gocov/internal/store"
)

func TestRepoBranchFilterAndPagination(t *testing.T) {
	f := newFixture(t, nil)
	doUpload(t, f, "secret-token", map[string]string{"commit": "main1", "branch": "main"}, testProfile)
	doUpload(t, f, "secret-token", map[string]string{"commit": "feat1", "branch": "feat"}, testProfile)

	body := get(f, "/repos/bitbucket/acme/widgets?branch=feat").Body.String()
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

	// One full page more on main, so main1 alone lands on page 1.
	for i := range uploadsPageSize {
		doUpload(t, f, "secret-token", map[string]string{
			"commit": "bulk" + strings.Repeat("x", i%3) + string(rune('a'+i%26)), "branch": "main",
		}, testProfile)
	}
	page0 := get(f, "/repos/bitbucket/acme/widgets?branch=main").Body.String()
	if !strings.Contains(page0, "Older") {
		t.Errorf("page 0 missing Older link")
	}
	page1 := get(f, "/repos/bitbucket/acme/widgets?branch=main&page=1").Body.String()
	if !strings.Contains(page1, "Newer") {
		t.Errorf("page 1 missing Newer link")
	}
	if !strings.Contains(page1, "main1") {
		t.Errorf("oldest upload not on the last page")
	}
}

// The unfiltered history reuses the branch-selector fetch instead of querying
// again. On the first page whose window needs one row more than that fetch
// holds, reusing it would hide "Older" with pages still to come.
func TestRepoPaginationPastRecentFetch(t *testing.T) {
	f := newFixture(t, nil)

	page := 0
	for (page+1)*uploadsPageSize+1 <= recentUploads {
		page++
	}
	for i := range (page + 2) * uploadsPageSize {
		u := &store.Upload{
			RepoID:    f.repo.ID,
			CommitSHA: fmt.Sprintf("c%04d", i),
			Branch:    "main",
			Format:    "go",
			TotalPct:  80,
		}
		if err := f.store.CreateUpload(t.Context(), u, nil); err != nil {
			t.Fatal(err)
		}
	}

	body := get(f, fmt.Sprintf("/repos/bitbucket/acme/widgets?page=%d", page)).Body.String()
	if !strings.Contains(body, "Older") {
		t.Errorf("page %d missing Older link with older uploads still to come", page)
	}
}

func TestRepoTrendChart(t *testing.T) {
	f := newFixture(t, nil)

	// One upload on the default branch: no chart section.
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main"}, testProfile)
	body := get(f, "/repos/bitbucket/acme/widgets").Body.String()
	if strings.Contains(body, `class="trend"`) {
		t.Errorf("trend rendered with a single upload")
	}

	// A PR upload must not count towards the two-point minimum.
	doUpload(t, f, "secret-token", map[string]string{"commit": "pr1", "branch": "main", "pr_id": "7"}, testProfile)
	body = get(f, "/repos/bitbucket/acme/widgets").Body.String()
	if strings.Contains(body, `class="trend"`) {
		t.Errorf("trend rendered counting a PR upload")
	}

	// Second branch upload, 100%: the chart appears, points link to uploads.
	better := "mode: set\nexample.com/m/a.go:1.1,5.2 10 3\n"
	doUpload(t, f, "secret-token", map[string]string{"commit": "c2", "branch": "main"}, better)
	body = get(f, "/repos/bitbucket/acme/widgets").Body.String()
	if !strings.Contains(body, `class="trend"`) {
		t.Fatalf("trend chart missing: %s", body)
	}
	if got := strings.Count(body, `<circle class="pt`); got != 2 {
		t.Errorf("marker count = %d, want 2 (PR upload excluded)", got)
	}
	if !strings.Contains(body, `<a href="/uploads/1"><circle`) {
		t.Errorf("trend points do not link to their uploads")
	}
	if strings.Contains(body, `class="pt fail"`) {
		t.Errorf("gate-fail marker rendered with no gate failures")
	}

	// A gate-failing upload gets the red marker.
	f.repo.Gate = store.Gate{MinCoverage: new(float64(90))}
	if err := f.store.UpdateRepo(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	doUpload(t, f, "secret-token", map[string]string{"commit": "c3", "branch": "main"}, testProfile) // 80%, fails
	body = get(f, "/repos/bitbucket/acme/widgets").Body.String()
	if !strings.Contains(body, `class="pt fail"`) {
		t.Errorf("gate-fail marker missing: %s", body)
	}

	// The branch filter drives the chart: feat has one upload, so no chart.
	doUpload(t, f, "secret-token", map[string]string{"commit": "f1", "branch": "feat"}, testProfile)
	body = get(f, "/repos/bitbucket/acme/widgets?branch=feat").Body.String()
	if strings.Contains(body, `class="trend"`) {
		t.Errorf("trend rendered for a branch with one upload")
	}
}

func TestRepoPageShowsFilesView(t *testing.T) {
	f := newFixture(t, nil)
	doUpload(t, f, "secret-token", map[string]string{"commit": "base1", "branch": "main"}, testProfileFull)
	doUpload(t, f, "secret-token", map[string]string{"commit": "main2", "branch": "main"}, testProfile)

	body := get(f, "/repos/bitbucket/acme/widgets").Body.String()
	for _, want := range []string{
		"Files on main",
		`id="view-mode"`,
		`data-view="tree"`,
		`data-view="list"`,
		`id="file-filters"`,
		`data-filter="all"`,
		`data-filter="changed"`,
		`data-filter="source"`,
		`data-filter="coverage"`,
		`id="file-search"`,
		`id="filetable-tree"`,
		`class="tree-row tree-dir`,
		`class="tree-row tree-file`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("repo page missing %q in response", want)
		}
	}
}

func TestReportBaseline(t *testing.T) {
	ok := func(id int64) *store.CommitReport { return trendReport(id, 80, "", false) }
	failed := func(id int64) *store.CommitReport { return trendReport(id, 50, "", true) }
	for _, tc := range []struct {
		name          string
		reports       []*store.CommitReport // newest first
		wantCur, want int64                 // report ids, 0 for nil
	}{
		{"no reports", nil, 0, 0},
		{"single report", []*store.CommitReport{ok(1)}, 1, 0},
		{"previous passed", []*store.CommitReport{ok(2), ok(1)}, 2, 1},
		// A failed report never becomes the baseline; the delta reads
		// against the last one that passed.
		{"skips failed", []*store.CommitReport{ok(3), failed(2), ok(1)}, 3, 1},
		{"all earlier failed", []*store.CommitReport{ok(3), failed(2), failed(1)}, 3, 0},
		// The newest report is the current one whether or not it passed.
		{"current failed", []*store.CommitReport{failed(2), ok(1)}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := func(r *store.CommitReport) int64 {
				if r == nil {
					return 0
				}
				return r.ID
			}
			cur, base := reportBaseline(tc.reports)
			if id(cur) != tc.wantCur || id(base) != tc.want {
				t.Errorf("reportBaseline = %d, %d, want %d, %d", id(cur), id(base), tc.wantCur, tc.want)
			}
		})
	}
}
