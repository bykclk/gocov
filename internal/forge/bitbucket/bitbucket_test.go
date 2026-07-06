package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestGetPRDiffNotImplemented(t *testing.T) {
	c := &Client{}
	_, err := c.GetPRDiff(context.Background(), "a/b", "1")
	if !errors.Is(err, forge.ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
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
