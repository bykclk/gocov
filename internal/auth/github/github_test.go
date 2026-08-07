package github

import (
	"context"
	"encoding/json"
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
	p.AuthBaseURL = srv.URL + "/login/oauth"
	p.APIBaseURL = srv.URL + "/api"
	return p
}

func TestAuthorizeURL(t *testing.T) {
	p := New("the-key", "the-secret")
	u, err := url.Parse(p.AuthorizeURL("state123", "https://gocov.example/oauth/github/callback"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.String(), DefaultAuthBaseURL+"/authorize?") {
		t.Errorf("url = %s", u)
	}
	q := u.Query()
	if q.Get("client_id") != "the-key" || q.Get("state") != "state123" ||
		q.Get("scope") != "read:org user:email" ||
		q.Get("redirect_uri") != "https://gocov.example/oauth/github/callback" {
		t.Errorf("query = %v", q)
	}
}

func TestIdentity(t *testing.T) {
	mux := http.NewServeMux()
	var apiBase string

	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("token exchange accept = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("client_id") != "the-key" || r.PostForm.Get("client_secret") != "the-secret" ||
			r.PostForm.Get("code") != "code99" {
			t.Errorf("form = %v", r.PostForm)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-abc"})
	})
	requireBearer := func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-abc" {
			t.Errorf("%s auth = %q", r.URL.Path, got)
		}
	}
	mux.HandleFunc("GET /api/user", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		// email null: private on the profile, served by /user/emails.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 12345, "login": "janedev", "name": "Jane Dev", "email": nil,
		})
	})
	mux.HandleFunc("GET /api/user/emails", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"email": "old@example.com", "primary": false},
			{"email": "jane@example.com", "primary": true},
		})
	})
	mux.HandleFunc("GET /api/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		requireBearer(r)
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"login": "acme"}})
			return
		}
		// First page links to a second one to exercise Link pagination.
		w.Header().Set("Link", "<"+apiBase+"/user/orgs?page=2>; rel=\"next\"")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"login": "widgets-inc"}})
	})

	p := testProvider(t, mux)
	apiBase = p.APIBaseURL

	id, err := p.Identity(context.Background(), "code99", "https://gocov.example/oauth/github/callback")
	if err != nil {
		t.Fatal(err)
	}
	if id.ForgeUUID != "12345" {
		t.Errorf("forge uuid = %q, want the numeric account id", id.ForgeUUID)
	}
	if id.DisplayName != "Jane Dev" || id.Email != "jane@example.com" {
		t.Errorf("identity = %+v", id)
	}
	// Orgs across both pages, plus the login for user-namespace repos.
	want := []string{"widgets-inc", "acme", "janedev"}
	if !reflect.DeepEqual(id.Workspaces, want) {
		t.Errorf("workspaces = %v, want %v", id.Workspaces, want)
	}
}

func TestIdentityPublicEmailSkipsEmailsCall(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-abc"})
	})
	mux.HandleFunc("GET /api/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "login": "solo", "email": "solo@example.com",
		})
	})
	mux.HandleFunc("GET /api/user/emails", func(w http.ResponseWriter, r *http.Request) {
		t.Error("emails must not be fetched when the profile has one")
	})
	mux.HandleFunc("GET /api/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})

	id, err := testProvider(t, mux).Identity(context.Background(), "c", "r")
	if err != nil {
		t.Fatal(err)
	}
	// No profile name: the login doubles as the display name; no orgs:
	// the login alone is the workspace list.
	if id.Email != "solo@example.com" || id.DisplayName != "solo" {
		t.Errorf("identity = %+v", id)
	}
	if !reflect.DeepEqual(id.Workspaces, []string{"solo"}) {
		t.Errorf("workspaces = %v, want just the login", id.Workspaces)
	}
}

func TestIdentityErrors(t *testing.T) {
	newMux := func(handlers map[string]http.HandlerFunc) *http.ServeMux {
		mux := http.NewServeMux()
		for pattern, h := range handlers {
			mux.HandleFunc(pattern, h)
		}
		return mux
	}
	tokenOK := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
	}

	t.Run("exchange http error", func(t *testing.T) {
		mux := newMux(map[string]http.HandlerFunc{
			"POST /login/oauth/access_token": func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "bad", http.StatusUnauthorized)
			},
		})
		if _, err := testProvider(t, mux).Identity(context.Background(), "c", "r"); err == nil {
			t.Error("want error")
		}
	})
	t.Run("exchange error in 200 body", func(t *testing.T) {
		// GitHub answers a bad code with 200 and an error field.
		mux := newMux(map[string]http.HandlerFunc{
			"POST /login/oauth/access_token": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
			},
		})
		_, err := testProvider(t, mux).Identity(context.Background(), "c", "r")
		if err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
			t.Errorf("err = %v, want the forge error surfaced", err)
		}
	})
	t.Run("user without id", func(t *testing.T) {
		mux := newMux(map[string]http.HandlerFunc{
			"POST /login/oauth/access_token": tokenOK,
			"GET /api/user": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"login": "x"})
			},
		})
		if _, err := testProvider(t, mux).Identity(context.Background(), "c", "r"); err == nil {
			t.Error("want error without an account id")
		}
	})
	t.Run("orgs listing fails", func(t *testing.T) {
		mux := newMux(map[string]http.HandlerFunc{
			"POST /login/oauth/access_token": tokenOK,
			"GET /api/user": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "x", "email": "x@y"})
			},
			"GET /api/user/orgs": func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", http.StatusForbidden)
			},
		})
		if _, err := testProvider(t, mux).Identity(context.Background(), "c", "r"); err == nil {
			t.Error("want error when memberships cannot be listed")
		}
	})
}
