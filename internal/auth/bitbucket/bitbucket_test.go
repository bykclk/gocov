package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// testProvider serves both the OAuth and API endpoints from one fake host.
func testProvider(t *testing.T, mux *http.ServeMux) *Provider {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := New("the-key", "the-secret")
	p.AuthBaseURL = srv.URL + "/site/oauth2"
	p.APIBaseURL = srv.URL + "/2.0"
	return p
}

func TestAuthorizeURL(t *testing.T) {
	p := New("the-key", "the-secret")
	u, err := url.Parse(p.AuthorizeURL("state123", "https://gocov.example/oauth/bitbucket/callback"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.String(), DefaultAuthBaseURL+"/authorize?") {
		t.Errorf("url = %s", u)
	}
	q := u.Query()
	if q.Get("client_id") != "the-key" || q.Get("response_type") != "code" ||
		q.Get("state") != "state123" ||
		q.Get("redirect_uri") != "https://gocov.example/oauth/bitbucket/callback" {
		t.Errorf("query = %v", q)
	}
}

func TestIdentity(t *testing.T) {
	mux := http.NewServeMux()
	var apiBase string

	mux.HandleFunc("POST /site/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "the-key" || pass != "the-secret" {
			t.Errorf("token exchange auth = %s:%s", user, pass)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "code99" {
			t.Errorf("form = %v", r.PostForm)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-abc"})
	})
	requireBearer := func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-abc" {
			t.Errorf("%s auth = %q", r.URL.Path, got)
		}
	}
	mux.HandleFunc("GET /2.0/user", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "{abc-123}", "display_name": "Jane Dev"})
	})
	mux.HandleFunc("GET /2.0/user/emails", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"values": []map[string]any{
			{"email": "old@example.com", "is_primary": false},
			{"email": "jane@example.com", "is_primary": true},
		}})
	})
	mux.HandleFunc("GET /2.0/user/workspaces", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"values": []map[string]any{{"workspace": map[string]any{"slug": "acme"}}},
			})
			return
		}
		// First page links to a second one to exercise pagination. The
		// membership objects nest the workspace, matching the real API.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values": []map[string]any{{"workspace": map[string]any{"slug": "personal"}}},
			"next":   apiBase + "/user/workspaces?page=2",
		})
	})

	p := testProvider(t, mux)
	apiBase = p.APIBaseURL

	id, err := p.Identity(context.Background(), "code99", "https://gocov.example/oauth/bitbucket/callback")
	if err != nil {
		t.Fatal(err)
	}
	if id.ForgeUUID != "{abc-123}" || id.DisplayName != "Jane Dev" || id.Email != "jane@example.com" {
		t.Errorf("identity = %+v", id)
	}
	if !reflect.DeepEqual(id.Workspaces, []string{"personal", "acme"}) {
		t.Errorf("workspaces = %v", id.Workspaces)
	}
}

func TestIdentityErrors(t *testing.T) {
	t.Run("token exchange failure", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /site/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		})
		p := testProvider(t, mux)
		if _, err := p.Identity(context.Background(), "bad", "uri"); err == nil ||
			!strings.Contains(err.Error(), "token exchange") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("api failure surfaces", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /site/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"access_token":"tok"}`)
		})
		mux.HandleFunc("GET /2.0/user", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusUnauthorized)
		})
		p := testProvider(t, mux)
		if _, err := p.Identity(context.Background(), "c", "uri"); err == nil ||
			!strings.Contains(err.Error(), "status 401") {
			t.Errorf("err = %v", err)
		}
	})
}
