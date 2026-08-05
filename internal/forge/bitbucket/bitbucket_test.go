package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
		BaseURL:     srv.URL,
		Username:    "user",
		AppPassword: "pass",
		HTTPClient:  srv.Client(),
	}
}

func TestPostBuildStatus(t *testing.T) {
	var gotPath, gotUser, gotPass string
	var gotBody map[string]string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
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
	if gotPath != "/repositories/acme/widgets/commit/abc123/statuses/build" {
		t.Errorf("path = %q", gotPath)
	}
	if gotUser != "user" || gotPass != "pass" {
		t.Errorf("basic auth = %q/%q", gotUser, gotPass)
	}
	if gotBody["state"] != "SUCCESSFUL" || gotBody["key"] != "gocov/coverage" ||
		gotBody["description"] != "coverage: 80.0% (+1.2%)" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestPostBuildStatusHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": "denied"}`, http.StatusForbidden)
	})
	err := c.PostBuildStatus(context.Background(), "a/b", "sha", forge.BuildStatus{State: forge.StateSuccessful})
	if err == nil {
		t.Fatal("want error on 403")
	}
}

func TestPostPRComment(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
	})
	if err := c.PostPRComment(context.Background(), "acme/widgets", "42", "hello"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repositories/acme/widgets/pullrequests/42/comments" {
		t.Errorf("path = %q", gotPath)
	}
	content, _ := gotBody["content"].(map[string]any)
	if content["raw"] != "hello" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestFindPRComment(t *testing.T) {
	// Comments are served newest first across two pages. The first match
	// must be the newest comment that is ours, top-level and not deleted:
	// bot-authored look-alikes by others, replies, inline and deleted
	// comments are all skipped.
	var srvURL string
	var gotSort string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"account_id": "bot-123", "uuid": "{b-uuid}"}`))
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"values": [
				{"id": 20, "user": {"account_id": "bot-123"}, "content": {"raw": "**gocov** report for old"}}
			]}`))
			return
		}
		gotSort = r.URL.Query().Get("sort")
		_, _ = w.Write([]byte(`{"values": [
			{"id": 55, "user": {"account_id": "intruder"}, "content": {"raw": "**gocov** fake capture"}},
			{"id": 54, "user": {"account_id": "bot-123"}, "parent": {"id": 40}, "content": {"raw": "**gocov** reply"}},
			{"id": 53, "user": {"account_id": "bot-123"}, "deleted": true, "content": {"raw": "**gocov** deleted"}},
			{"id": 52, "user": {"account_id": "bot-123"}, "inline": {"path": "a.go"}, "content": {"raw": "**gocov** inline"}},
			{"id": 51, "user": {"account_id": "bot-123"}, "content": {"raw": "**gocov** report for new"}}
		], "next": "` + srvURL + r.URL.Path + `?page=2"}`))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	srvURL = srv.URL
	c := &Client{BaseURL: srv.URL, Username: "u", AppPassword: "p", HTTPClient: srv.Client()}

	id, err := c.FindPRComment(context.Background(), "acme/widgets", "42", "**gocov**")
	if err != nil {
		t.Fatal(err)
	}
	if id != "51" {
		t.Errorf("id = %q, want 51 (newest own top-level match)", id)
	}
	if gotSort != "-created_on" {
		t.Errorf("sort = %q, want -created_on", gotSort)
	}
}

func TestFindPRCommentNoMatch(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"account_id": "bot-123"}`))
			return
		}
		_, _ = w.Write([]byte(`{"values": [{"id": 1, "user": {"account_id": "bot-123"}, "content": {"raw": "hi"}}]}`))
	})
	id, err := c.FindPRComment(context.Background(), "a/b", "1", "**gocov**")
	if err != nil || id != "" {
		t.Errorf("id, err = %q, %v; want empty, nil", id, err)
	}
}

func TestFindPRCommentIdentityFailure(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		t.Error("comments must not be listed when identity is unknown")
	})
	if _, err := c.FindPRComment(context.Background(), "a/b", "1", "**gocov**"); err == nil {
		t.Error("want error when /user fails")
	}
}

func TestUpdatePRComment(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
	})
	if err := c.UpdatePRComment(context.Background(), "acme/widgets", "42", "31", "new body"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/repositories/acme/widgets/pullrequests/42/comments/31" {
		t.Errorf("%s %s", gotMethod, gotPath)
	}
	content, _ := gotBody["content"].(map[string]any)
	if content["raw"] != "new body" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestGetPRDiff(t *testing.T) {
	const diff = "--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n x\n+y\n"
	var gotPath, gotUser string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, _, _ = r.BasicAuth()
		_, _ = w.Write([]byte(diff))
	})
	got, err := c.GetPRDiff(context.Background(), "acme/widgets", "42")
	if err != nil {
		t.Fatal(err)
	}
	if got != diff {
		t.Errorf("diff = %q", got)
	}
	if gotPath != "/repositories/acme/widgets/pullrequests/42/diff" {
		t.Errorf("path = %q", gotPath)
	}
	if gotUser != "user" {
		t.Errorf("basic auth user = %q", gotUser)
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
		_, _ = w.Write([]byte(`{"mainbranch": {"name": "development"}, "slug": "widgets"}`))
	})
	got, err := c.GetDefaultBranch(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got != "development" {
		t.Errorf("branch = %q", got)
	}
	if gotPath != "/repositories/acme/widgets" {
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
	t.Run("missing mainbranch", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"slug": "b"}`))
		})
		if _, err := c.GetDefaultBranch(context.Background(), "a/b"); err == nil {
			t.Error("want error when mainbranch is absent")
		}
	})
}

func TestFactoryValidation(t *testing.T) {
	if _, err := Factory(map[string]string{"username": "u"}); err == nil {
		t.Error("want error without app_password")
	}
	if _, err := Factory(nil); err == nil {
		t.Error("want error without credentials")
	}
	f, err := Factory(map[string]string{"username": "u", "app_password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if f.(*Client).BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %q", f.(*Client).BaseURL)
	}
}

func TestPublishReport(t *testing.T) {
	type call struct {
		method, path string
		body         []byte
	}
	var calls []call
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, body})
		if r.Method == http.MethodDelete {
			// First publish: nothing to delete yet.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
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
		{Path: "m/a.go", Line: 9, Summary: "Line 9 of this change is not covered by tests"},
		{Path: "m/b.go", Line: 4, Summary: "Lines 4–6 of this change are not covered by tests"},
	}
	if err := c.PublishReport(context.Background(), "acme/widgets", "abc123", report, annotations); err != nil {
		t.Fatal(err)
	}

	base := "/repositories/acme/widgets/commit/abc123/reports/gocov"
	if len(calls) != 3 {
		t.Fatalf("got %d requests, want 3 (DELETE, PUT, POST)", len(calls))
	}
	if calls[0].method != http.MethodDelete || calls[0].path != base {
		t.Errorf("call[0] = %s %s, want DELETE %s", calls[0].method, calls[0].path, base)
	}
	if calls[1].method != http.MethodPut || calls[1].path != base {
		t.Errorf("call[1] = %s %s, want PUT %s", calls[1].method, calls[1].path, base)
	}
	if calls[2].method != http.MethodPost || calls[2].path != base+"/annotations" {
		t.Errorf("call[2] = %s %s, want POST %s/annotations", calls[2].method, calls[2].path, base)
	}

	var putBody map[string]any
	if err := json.Unmarshal(calls[1].body, &putBody); err != nil {
		t.Fatal(err)
	}
	if putBody["report_type"] != "COVERAGE" || putBody["result"] != "PASSED" ||
		putBody["reporter"] != "gocov" || putBody["title"] != "gocov coverage" ||
		putBody["link"] != "https://gocov.example/uploads/1" {
		t.Errorf("report payload = %v", putBody)
	}
	data, _ := putBody["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("data fields = %d, want 3", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["type"] != "PERCENTAGE" || first["title"] != "Total coverage" || first["value"] != 80.0 {
		t.Errorf("data[0] = %v", first)
	}
	if second, _ := data[1].(map[string]any); second["type"] != "NUMBER" {
		t.Errorf("data[1] = %v", second)
	}
	if third, _ := data[2].(map[string]any); third["type"] != "TEXT" || third["value"] != "8 / 10" {
		t.Errorf("data[2] = %v", third)
	}

	var annBody []map[string]any
	if err := json.Unmarshal(calls[2].body, &annBody); err != nil {
		t.Fatal(err)
	}
	if len(annBody) != 2 {
		t.Fatalf("annotations = %d, want 2", len(annBody))
	}
	a := annBody[0]
	if a["external_id"] != "gocov-001" || a["annotation_type"] != "CODE_SMELL" ||
		a["severity"] != "MEDIUM" || a["path"] != "m/a.go" || a["line"] != 9.0 ||
		a["summary"] != "Line 9 of this change is not covered by tests" {
		t.Errorf("annotation[0] = %v", a)
	}
	if annBody[1]["external_id"] != "gocov-002" {
		t.Errorf("annotation[1] external_id = %v", annBody[1]["external_id"])
	}
}

func TestPublishReportWithoutAnnotations(t *testing.T) {
	var methods []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		// Existing report from an earlier upload: delete succeeds.
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	report := forge.Report{Title: "gocov coverage", Details: "d"}
	if err := c.PublishReport(context.Background(), "acme/widgets", "abc123", report, nil); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodDelete || methods[1] != http.MethodPut {
		t.Errorf("methods = %v, want [DELETE PUT] and no annotation POST", methods)
	}
}

func TestPublishReportErrors(t *testing.T) {
	t.Run("delete fails", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		err := c.PublishReport(context.Background(), "a/b", "c", forge.Report{}, nil)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v, want 500", err)
		}
	})
	t.Run("put fails", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			http.Error(w, "bad", http.StatusBadRequest)
		})
		err := c.PublishReport(context.Background(), "a/b", "c", forge.Report{}, nil)
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("err = %v, want 400", err)
		}
	})
	t.Run("unknown data type", func(t *testing.T) {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		err := c.PublishReport(context.Background(), "a/b", "c", forge.Report{
			Data: []forge.ReportData{{Title: "x", Type: "bogus", Value: 1}},
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Errorf("err = %v, want unknown data type", err)
		}
	})
}
