package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bykclk/gocov/internal/forge"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		BaseURL:    srv.URL,
		Token:      "tok",
		HTTPClient: srv.Client(),
	}
}

func TestPostBuildStatus(t *testing.T) {
	var gotPath, gotAuth, gotVersion string
	var gotBody map[string]string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})

	err := c.PostBuildStatus(context.Background(), "acme/widgets", "abc123", forge.BuildStatus{
		Key:         "gocov/coverage",
		State:       forge.StateSuccessful,
		Name:        "gocov",
		Description: "coverage: 80.0% (+1.2%)",
		URL:         "https://gocov.example/uploads/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/acme/widgets/statuses/abc123" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotVersion == "" {
		t.Error("X-GitHub-Api-Version header missing")
	}
	if gotBody["state"] != "success" || gotBody["context"] != "gocov" ||
		gotBody["description"] != "coverage: 80.0% (+1.2%)" ||
		gotBody["target_url"] != "https://gocov.example/uploads/1" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestPostBuildStatusStates(t *testing.T) {
	var gotState string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotState = body["state"]
		w.WriteHeader(http.StatusCreated)
	})
	for state, want := range map[string]string{
		forge.StateSuccessful: "success",
		forge.StateFailed:     "failure",
		forge.StateInProgress: "pending",
	} {
		if err := c.PostBuildStatus(context.Background(), "a/b", "sha", forge.BuildStatus{State: state, Name: "gocov"}); err != nil {
			t.Fatal(err)
		}
		if gotState != want {
			t.Errorf("state %q mapped to %q, want %q", state, gotState, want)
		}
	}
	if err := c.PostBuildStatus(context.Background(), "a/b", "sha", forge.BuildStatus{State: "bogus"}); err == nil {
		t.Error("want error for unknown state")
	}
}

func TestPostBuildStatusTruncatesDescription(t *testing.T) {
	var gotBody map[string]string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})
	long := strings.Repeat("cover ", 40) // 240 chars
	err := c.PostBuildStatus(context.Background(), "a/b", "sha", forge.BuildStatus{
		State: forge.StateFailed, Name: "gocov", Description: long,
	})
	if err != nil {
		t.Fatal(err)
	}
	desc := []rune(gotBody["description"])
	if len(desc) != statusMaxDescription {
		t.Errorf("description length = %d runes, want %d", len(desc), statusMaxDescription)
	}
	if desc[len(desc)-1] != '…' {
		t.Errorf("truncated description does not end in ellipsis: %q", gotBody["description"])
	}
}

func TestPostBuildStatusHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "denied"}`, http.StatusForbidden)
	})
	err := c.PostBuildStatus(context.Background(), "a/b", "sha", forge.BuildStatus{State: forge.StateSuccessful})
	if err == nil {
		t.Fatal("want error on 403")
	}
}

func TestPostPRComment(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})
	if err := c.PostPRComment(context.Background(), "acme/widgets", "42", "hello"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/acme/widgets/issues/42/comments" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["body"] != "hello" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestFindPRComment(t *testing.T) {
	// GitHub lists issue comments oldest first across pages; the newest
	// match — the last one seen — must win, and non-matching bodies are
	// skipped.
	var srvURL string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[
				{"id": 30, "body": "**gocov** report for new"},
				{"id": 31, "body": "unrelated"}
			]`))
			return
		}
		w.Header().Set("Link", "<"+srvURL+r.URL.Path+"?page=2>; rel=\"next\"")
		_, _ = w.Write([]byte(`[
			{"id": 10, "body": "**gocov** report for old"},
			{"id": 11, "body": "a human comment"}
		]`))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	srvURL = srv.URL
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()}

	id, err := c.FindPRComment(context.Background(), "acme/widgets", "42", "**gocov**")
	if err != nil {
		t.Fatal(err)
	}
	if id != "30" {
		t.Errorf("id = %q, want 30 (newest match)", id)
	}
}

func TestFindPRCommentNoMatch(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "body": "hi"}]`))
	})
	id, err := c.FindPRComment(context.Background(), "a/b", "1", "**gocov**")
	if err != nil || id != "" {
		t.Errorf("id, err = %q, %v; want empty, nil", id, err)
	}
}

func TestFindPRCommentHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	if _, err := c.FindPRComment(context.Background(), "a/b", "1", "**gocov**"); err == nil {
		t.Error("want error on 401")
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`,
			"https://api.github.com/x?page=2"},
		{`<https://api.github.com/x?page=1>; rel="prev"`, ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := nextLink(tt.header); got != tt.want {
			t.Errorf("nextLink(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestUpdatePRComment(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
	})
	if err := c.UpdatePRComment(context.Background(), "acme/widgets", "42", "31", "new body"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/repos/acme/widgets/issues/comments/31" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	if gotBody["body"] != "new body" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestGetPRDiff(t *testing.T) {
	const diff = "--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n x\n+y\n"
	var gotPath, gotAccept string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(diff))
	})
	got, err := c.GetPRDiff(context.Background(), "acme/widgets", "42")
	if err != nil {
		t.Fatal(err)
	}
	if got != diff {
		t.Errorf("diff = %q", got)
	}
	if gotPath != "/repos/acme/widgets/pulls/42" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAccept != "application/vnd.github.v3.diff" {
		t.Errorf("accept = %q", gotAccept)
	}
}

func TestGetPRDiffHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	if _, err := c.GetPRDiff(context.Background(), "a/b", "1"); err == nil {
		t.Error("want error on 404")
	}
}

func TestGetDefaultBranch(t *testing.T) {
	var gotPath string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"default_branch": "develop", "name": "widgets"}`))
	})
	got, err := c.GetDefaultBranch(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got != "develop" {
		t.Errorf("branch = %q", got)
	}
	if gotPath != "/repos/acme/widgets" {
		t.Errorf("path = %q", gotPath)
	}
}

func TestGetDefaultBranchErrors(t *testing.T) {
	t.Run("404 maps to ErrRepoNotFound", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})
		_, err := c.GetDefaultBranch(context.Background(), "a/ghost")
		if !errors.Is(err, forge.ErrRepoNotFound) {
			t.Errorf("err = %v, want ErrRepoNotFound", err)
		}
	})
	t.Run("http error", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		})
		if _, err := c.GetDefaultBranch(context.Background(), "a/b"); err == nil {
			t.Error("want error on 403")
		}
	})
	t.Run("missing default_branch", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"name": "b"}`))
		})
		if _, err := c.GetDefaultBranch(context.Background(), "a/b"); err == nil {
			t.Error("want error when default_branch is absent")
		}
	})
}

func TestGetFileContent(t *testing.T) {
	var gotPath, gotRef, gotAccept string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRef = r.URL.Query().Get("ref")
		gotAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("package main\n"))
	})
	got, err := c.GetFileContent(context.Background(), "acme/widgets", "abc123", "cmd/app/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package main\n" {
		t.Errorf("content = %q", got)
	}
	if gotPath != "/repos/acme/widgets/contents/cmd/app/main.go" {
		t.Errorf("path = %q", gotPath)
	}
	if gotRef != "abc123" {
		t.Errorf("ref = %q", gotRef)
	}
	if gotAccept != "application/vnd.github.raw+json" {
		t.Errorf("accept = %q", gotAccept)
	}
}

func TestGetFileContentNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	_, err := c.GetFileContent(context.Background(), "a/b", "sha", "ghost.go")
	if !errors.Is(err, forge.ErrRepoNotFound) {
		t.Errorf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestPublishReport(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id": 77}`))
	})

	report := forge.Report{
		Title:   "gocov coverage",
		Details: "2 of 3 changed lines are covered by tests.",
		Result:  forge.ReportPassed,
		Link:    "https://gocov.example/uploads/1",
		Data: []forge.ReportData{
			{Title: "Total coverage", Type: forge.DataPercentage, Value: 80.0},
			{Title: "Uncovered changed lines", Type: forge.DataNumber, Value: 1.0},
			{Title: "Statements", Type: forge.DataText, Value: "8 / 10"},
		},
	}
	annotations := []forge.Annotation{
		{Path: "m/a.go", Line: 9, EndLine: 11, Summary: "Lines 9–11 of this change are not covered by tests"},
		{Path: "m/b.go", Summary: "This changed file has no coverage data — nothing in it appears to be tested"},
	}
	if err := c.PublishReport(context.Background(), "acme/widgets", "abc123", report, annotations); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost || gotPath != "/repos/acme/widgets/check-runs" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	if gotBody["name"] != "gocov coverage" || gotBody["head_sha"] != "abc123" ||
		gotBody["status"] != "completed" || gotBody["conclusion"] != "success" ||
		gotBody["details_url"] != "https://gocov.example/uploads/1" ||
		gotBody["external_id"] != "gocov" {
		t.Errorf("payload = %v", gotBody)
	}
	output, _ := gotBody["output"].(map[string]any)
	if output["title"] != "2 of 3 changed lines are covered by tests." {
		t.Errorf("output title = %v", output["title"])
	}
	summary, _ := output["summary"].(string)
	for _, want := range []string{
		"2 of 3 changed lines are covered by tests.",
		"| Total coverage | 80.0% |",
		"| Uncovered changed lines | 1 |",
		"| Statements | 8 / 10 |",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	anns, _ := output["annotations"].([]any)
	if len(anns) != 2 {
		t.Fatalf("annotations = %v", anns)
	}
	first, _ := anns[0].(map[string]any)
	if first["path"] != "m/a.go" || first["start_line"] != 9.0 || first["end_line"] != 11.0 ||
		first["annotation_level"] != "warning" ||
		first["message"] != "Lines 9–11 of this change are not covered by tests" {
		t.Errorf("annotation[0] = %v", first)
	}
	// File-level finding: the Checks API demands lines, so it anchors at 1.
	second, _ := anns[1].(map[string]any)
	if second["start_line"] != 1.0 || second["end_line"] != 1.0 {
		t.Errorf("annotation[1] = %v", second)
	}
}

func TestPublishReportConclusions(t *testing.T) {
	var gotConclusion string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotConclusion, _ = body["conclusion"].(string)
		_, _ = w.Write([]byte(`{"id": 1}`))
	})
	for result, want := range map[string]string{
		forge.ReportPassed: "success",
		forge.ReportFailed: "failure",
		"":                 "neutral",
	} {
		if err := c.PublishReport(context.Background(), "a/b", "sha", forge.Report{Title: "t", Result: result}, nil); err != nil {
			t.Fatal(err)
		}
		if gotConclusion != want {
			t.Errorf("result %q mapped to %q, want %q", result, gotConclusion, want)
		}
	}
	if err := c.PublishReport(context.Background(), "a/b", "sha", forge.Report{Result: "bogus"}, nil); err == nil {
		t.Error("want error for unknown result")
	}
}

func TestPublishReportBatchesAnnotations(t *testing.T) {
	// 120 annotations: 50 ride on the create, the rest arrive in two
	// update batches appended to the created run.
	type call struct {
		method, path string
		count        int
	}
	var calls []call
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		output, _ := body["output"].(map[string]any)
		anns, _ := output["annotations"].([]any)
		calls = append(calls, call{r.Method, r.URL.Path, len(anns)})
		_, _ = w.Write([]byte(`{"id": 77}`))
	})

	annotations := make([]forge.Annotation, 120)
	for i := range annotations {
		annotations[i] = forge.Annotation{Path: "a.go", Line: i + 1, Summary: "s"}
	}
	if err := c.PublishReport(context.Background(), "acme/widgets", "abc", forge.Report{Title: "t"}, annotations); err != nil {
		t.Fatal(err)
	}

	want := []call{
		{http.MethodPost, "/repos/acme/widgets/check-runs", 50},
		{http.MethodPatch, "/repos/acme/widgets/check-runs/77", 50},
		{http.MethodPatch, "/repos/acme/widgets/check-runs/77", 20},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestPublishReportForbiddenMapsToNotImplemented(t *testing.T) {
	// Check-run writes are closed to classic tokens; the upload flow
	// reports the sentinel as "skipped" instead of an error.
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "Resource not accessible by personal access token"}`, http.StatusForbidden)
	})
	err := c.PublishReport(context.Background(), "a/b", "sha", forge.Report{Title: "t"}, nil)
	if !errors.Is(err, forge.ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "check runs") {
		t.Errorf("err = %v, want a check-run explanation", err)
	}
}

func TestPublishReportHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	err := c.PublishReport(context.Background(), "a/b", "sha", forge.Report{Title: "t"}, nil)
	if err == nil || errors.Is(err, forge.ErrNotImplemented) {
		t.Errorf("err = %v, want a surfaced 500", err)
	}
}

func TestFactoryValidation(t *testing.T) {
	if _, err := Factory(nil); err == nil {
		t.Error("want error without credentials")
	}
	if _, err := Factory(map[string]string{"username": "u"}); err == nil {
		t.Error("want error without token")
	}
	f, err := Factory(map[string]string{"token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if f.(*Client).BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %q", f.(*Client).BaseURL)
	}
}
