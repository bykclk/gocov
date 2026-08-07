package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/diffcov"
	"github.com/bykclk/gocov/internal/forge"
	"github.com/bykclk/gocov/internal/store"
)

// coveredPRDiff touches only a.go lines 2-3, which the profile covers, so
// diff coverage is 100% and no annotations are expected.
const coveredPRDiff = `diff --git a/m/a.go b/m/a.go
--- a/m/a.go
+++ b/m/a.go
@@ -1,3 +1,5 @@
 ctx
+added 2
+added 3
 ctx
 ctx
`

func insightsUpload(t *testing.T, f *fixture, fields map[string]string) uploadResponse {
	t.Helper()
	rec := doUpload(t, f, "secret-token", fields, testProfile)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCodeInsightsPRUpload(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	f.repo.Gate = store.Gate{MinCoverage: pctPtr(50)}
	if err := f.store.UpdateRepo(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	f.forge.DiffText = testPRDiff

	fields := map[string]string{"commit": "prcommit1", "branch": "feature/x", "pr_id": "42"}
	resp := insightsUpload(t, f, fields)
	if resp.CodeInsights != "posted" {
		t.Fatalf("code_insights = %q, want posted", resp.CodeInsights)
	}
	if len(f.forge.ReportCalls) != 1 {
		t.Fatalf("report calls = %d, want 1", len(f.forge.ReportCalls))
	}
	call := f.forge.ReportCalls[0]
	if call.RepoSlug != "acme/widgets" || call.CommitSHA != "prcommit1" {
		t.Errorf("report call target = %s@%s", call.RepoSlug, call.CommitSHA)
	}
	r := call.Report
	if r.Title != "gocov coverage" || r.Result != forge.ReportPassed {
		t.Errorf("report = %q result %q", r.Title, r.Result)
	}
	if !strings.Contains(r.Link, "/uploads/") {
		t.Errorf("report link = %q, want upload page link", r.Link)
	}
	if !strings.Contains(r.Details, "2 of 3 changed lines are covered") {
		t.Errorf("report details = %q", r.Details)
	}

	// Data fields in PRD order: total, diff pct, uncovered count,
	// statements, gate (no delta on a first upload), then the per-file
	// summary filling the remaining budget.
	titles := make([]string, len(r.Data))
	for i, d := range r.Data {
		titles[i] = d.Title
	}
	want := []string{"Total coverage", "Diff coverage", "Uncovered changed lines", "Statements", "Gate", "m/a.go"}
	if strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("data fields = %v, want %v", titles, want)
	}
	if r.Data[5].Type != forge.DataPercentage {
		t.Errorf("per-file field = %+v", r.Data[5])
	}
	if r.Data[0].Type != forge.DataPercentage || r.Data[0].Value != 80.0 {
		t.Errorf("total coverage field = %+v", r.Data[0])
	}
	if got := r.Data[1].Value.(float64); math.Abs(got-200.0/3.0) > 1e-9 {
		t.Errorf("diff coverage = %v, want 66.67", got)
	}
	if r.Data[2].Type != forge.DataNumber || r.Data[2].Value != 1.0 {
		t.Errorf("uncovered changed lines field = %+v", r.Data[2])
	}
	if r.Data[3].Value != "8 / 10" || r.Data[4].Value != "passed" {
		t.Errorf("statements/gate = %v / %v", r.Data[3].Value, r.Data[4].Value)
	}

	// Two annotations: the whole-file finding for untested.go (file
	// level, no line) comes first, then a.go line 9 — the only
	// uncovered changed line with coverage data.
	if len(call.Annotations) != 2 {
		t.Fatalf("annotations = %+v, want exactly 2", call.Annotations)
	}
	if a := call.Annotations[0]; a.Path != "m/untested.go" || a.Line != 0 ||
		!strings.Contains(a.Summary, "no coverage data") {
		t.Errorf("file-level annotation = %+v", a)
	}
	if a := call.Annotations[1]; a.Path != "m/a.go" || a.Line != 9 ||
		a.Summary != "Line 9 of this change is not covered by tests" {
		t.Errorf("line annotation = %+v", a)
	}

	// Re-upload for the same commit publishes again under the same
	// commit key (the forge replaces in place) and now carries a delta
	// against the first, gate-passing upload.
	resp = insightsUpload(t, f, fields)
	if resp.CodeInsights != "posted" || len(f.forge.ReportCalls) != 2 {
		t.Fatalf("re-upload: code_insights = %q, %d report calls", resp.CodeInsights, len(f.forge.ReportCalls))
	}
	second := f.forge.ReportCalls[1]
	if second.CommitSHA != "prcommit1" {
		t.Errorf("re-upload commit = %q", second.CommitSHA)
	}
	if len(second.Report.Data) < 2 || second.Report.Data[1].Title != "Change vs base" ||
		second.Report.Data[1].Value != "+0.0%" {
		t.Errorf("re-upload delta field = %+v", second.Report.Data)
	}
}

func TestCodeInsightsFullyCoveredDiff(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	f.repo.Gate = store.Gate{MinDiffCoverage: pctPtr(100)}
	if err := f.store.UpdateRepo(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	f.forge.DiffText = coveredPRDiff

	resp := insightsUpload(t, f, map[string]string{"commit": "c1", "branch": "feature/x", "pr_id": "7"})
	if resp.CodeInsights != "posted" {
		t.Fatalf("code_insights = %q", resp.CodeInsights)
	}
	call := f.forge.ReportCalls[0]
	if call.Report.Result != forge.ReportPassed {
		t.Errorf("result = %q, want passed at 100%% diff coverage", call.Report.Result)
	}
	if len(call.Annotations) != 0 {
		t.Errorf("annotations = %+v, want none", call.Annotations)
	}
	for _, d := range call.Report.Data {
		if d.Title == "Uncovered changed lines" && d.Value != 0.0 {
			t.Errorf("uncovered changed lines = %v, want 0", d.Value)
		}
	}
}

func TestCodeInsightsNonPRUploadHasNoAnnotations(t *testing.T) {
	f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})

	resp := insightsUpload(t, f, map[string]string{"commit": "c1", "branch": "main"})
	if resp.CodeInsights != "posted" {
		t.Fatalf("code_insights = %q", resp.CodeInsights)
	}
	call := f.forge.ReportCalls[0]
	if len(call.Annotations) != 0 {
		t.Errorf("annotations on a non-PR upload: %+v", call.Annotations)
	}
	// No gate configured: data without a verdict.
	if call.Report.Result != "" {
		t.Errorf("result = %q, want no verdict without a gate", call.Report.Result)
	}
	for _, d := range call.Report.Data {
		if d.Title == "Diff coverage" || d.Title == "Gate" {
			t.Errorf("unexpected data field %q on a non-PR, no-gate upload", d.Title)
		}
	}
}

func TestCodeInsightsSkippedWithoutCredentials(t *testing.T) {
	f := newFixture(t, nil)

	resp := insightsUpload(t, f, map[string]string{"commit": "c1"})
	if resp.CodeInsights != "skipped" {
		t.Errorf("code_insights = %q, want skipped", resp.CodeInsights)
	}
	if len(f.forge.ReportCalls) != 0 {
		t.Errorf("report calls without credentials: %+v", f.forge.ReportCalls)
	}
}

// TestCodeInsightsSkippedWithReason covers a forge that wraps
// ErrNotImplemented with an explanation — e.g. GitHub check runs being
// closed to the credential type — which the response surfaces instead
// of a bare "skipped".
func TestCodeInsightsSkippedWithReason(t *testing.T) {
	f := newFixture(t, map[string]string{"token": "t"})
	f.forge.ReportErr = fmt.Errorf("github: %w: this GitHub credential cannot write check runs", forge.ErrNotImplemented)

	resp := insightsUpload(t, f, map[string]string{"commit": "c1"})
	want := "skipped: github: " + forge.ErrNotImplemented.Error() + ": this GitHub credential cannot write check runs"
	if resp.CodeInsights != want {
		t.Errorf("code_insights = %q, want %q", resp.CodeInsights, want)
	}
}

// TestCodeInsightsFailureIsolation stubs the insights push to fail and
// verifies the rest of the upload — response, build status, PR comment —
// is identical to a healthy run.
func TestCodeInsightsFailureIsolation(t *testing.T) {
	fields := map[string]string{"commit": "prcommit1", "branch": "feature/x", "pr_id": "42"}

	healthy := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	healthy.forge.DiffText = testPRDiff
	wantResp := insightsUpload(t, healthy, fields)

	broken := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
	broken.forge.DiffText = testPRDiff
	broken.forge.ReportErr = errors.New("bitbucket: reports returned 502: bad gateway")
	resp := insightsUpload(t, broken, fields)

	if resp.CodeInsights != "error: bitbucket: reports returned 502: bad gateway" {
		t.Errorf("code_insights = %q", resp.CodeInsights)
	}
	resp.CodeInsights = wantResp.CodeInsights
	got, _ := json.Marshal(resp)
	want, _ := json.Marshal(wantResp)
	if string(got) != string(want) {
		t.Errorf("upload response diverged beyond code_insights:\ngot  %s\nwant %s", got, want)
	}
	if len(broken.forge.StatusCalls) != 1 ||
		broken.forge.StatusCalls[0] != healthy.forge.StatusCalls[0] {
		t.Errorf("build status diverged: %+v", broken.forge.StatusCalls)
	}
	if len(broken.forge.CommentCalls) != 1 ||
		broken.forge.CommentCalls[0].Body != healthy.forge.CommentCalls[0].Body {
		t.Errorf("PR comment diverged")
	}
}

func TestInsightsAnnotationsCollapseRanges(t *testing.T) {
	dc := &diffcov.Result{
		Files: []diffcov.FileCoverage{
			{Path: "a.go", UncoveredLines: []int{5, 6, 7, 10}},
			{Path: "b.go", UncoveredLines: []int{1}},
		},
		UnmatchedFiles: []string{"c.go"},
	}
	anns, dropped := insightsAnnotations(dc)
	if dropped != 0 {
		t.Fatalf("dropped = %d", dropped)
	}
	want := []forge.Annotation{
		{Path: "c.go", Summary: "This changed file has no coverage data — nothing in it appears to be tested"},
		{Path: "a.go", Line: 5, EndLine: 7, Summary: "Lines 5–7 of this change are not covered by tests"},
		{Path: "a.go", Line: 10, EndLine: 10, Summary: "Line 10 of this change is not covered by tests"},
		{Path: "b.go", Line: 1, EndLine: 1, Summary: "Line 1 of this change is not covered by tests"},
	}
	if len(anns) != len(want) {
		t.Fatalf("annotations = %+v", anns)
	}
	for i := range want {
		if anns[i] != want[i] {
			t.Errorf("annotation[%d] = %+v, want %+v", i, anns[i], want[i])
		}
	}
}

func TestInsightsAnnotationsTruncate(t *testing.T) {
	// 105 non-contiguous uncovered lines: odd numbers 1..209.
	lines := make([]int, 105)
	for i := range lines {
		lines[i] = 2*i + 1
	}
	dc := &diffcov.Result{
		Files:        []diffcov.FileCoverage{{Path: "big.go", UncoveredLines: lines}},
		TotalLines:   105,
		CoveredLines: 0,
	}
	anns, dropped := insightsAnnotations(dc)
	if len(anns) != insightsMaxAnnotations || dropped != 5 {
		t.Fatalf("got %d annotations, %d dropped; want 100/5", len(anns), dropped)
	}

	// The truncation is called out in the report details.
	f := newFixture(t, nil)
	report, _ := f.srv.insightsReport(&store.Upload{
		TotalPct: 80, CoveredStmts: 8, TotalStmts: 10, DiffCoverage: dc,
	}, nil, gateResult{})
	if !strings.Contains(report.Details, "+5 more uncovered ranges") {
		t.Errorf("details = %q, want truncation note", report.Details)
	}
}

func TestInsightsPerFileDataBudget(t *testing.T) {
	// Eight partially covered files against six standard fields (delta
	// and gate present): the per-file summary must stop at the API's
	// ten-field cap, worst-covered file first.
	files := make([]diffcov.FileCoverage, 8)
	for i := range files {
		files[i] = diffcov.FileCoverage{
			Path:           string(rune('a'+i)) + ".go",
			CoveredLines:   int64(i), // i of 10 covered: a.go worst
			TotalLines:     10,
			UncoveredLines: []int{1}, // marker: has uncovered lines
		}
	}
	dc := &diffcov.Result{Files: files, CoveredLines: 28, TotalLines: 80}

	f := newFixture(t, nil)
	delta := 1.5
	report, _ := f.srv.insightsReport(&store.Upload{
		TotalPct: 80, CoveredStmts: 8, TotalStmts: 10, DiffCoverage: dc,
	}, &delta, gateResult{configured: true})

	if len(report.Data) != insightsMaxDataFields {
		t.Fatalf("data fields = %d, want %d", len(report.Data), insightsMaxDataFields)
	}
	// Six standard fields, then the four worst files a.go .. d.go.
	for i, wantTitle := range []string{"a.go", "b.go", "c.go", "d.go"} {
		d := report.Data[6+i]
		if d.Title != wantTitle || d.Type != forge.DataPercentage || d.Value != float64(i*10) {
			t.Errorf("per-file field[%d] = %+v, want %s at %d%%", i, d, wantTitle, i*10)
		}
	}
}

func TestInsightsFullyCoveredFilesClaimNoDataFields(t *testing.T) {
	dc := &diffcov.Result{
		Files: []diffcov.FileCoverage{
			{Path: "ok.go", CoveredLines: 5, TotalLines: 5},
			{Path: "bad.go", CoveredLines: 0, TotalLines: 2, UncoveredLines: []int{3, 4}},
		},
		CoveredLines: 5, TotalLines: 7,
	}
	f := newFixture(t, nil)
	report, _ := f.srv.insightsReport(&store.Upload{
		TotalPct: 80, CoveredStmts: 8, TotalStmts: 10, DiffCoverage: dc,
	}, nil, gateResult{})
	for _, d := range report.Data {
		if d.Title == "ok.go" {
			t.Errorf("fully covered file claimed a data field: %+v", d)
		}
	}
	last := report.Data[len(report.Data)-1]
	if last.Title != "bad.go" || last.Value != 0.0 {
		t.Errorf("per-file field = %+v, want bad.go at 0%%", last)
	}
}
