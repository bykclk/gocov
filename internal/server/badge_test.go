package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
			req := httptest.NewRequest(http.MethodGet, "/badge/bitbucket/acme/widgets.svg", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/badge/bitbucket/no/such.svg", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/badge/bitbucket/acme/widgets.svg", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), ">unknown<") {
		t.Errorf("badge should be unknown without default-branch uploads: %s", rec.Body)
	}
}

func TestBadgeAndDashboardShowMergedTotal(t *testing.T) {
	f := newFixture(t, nil)
	// Backend part (8/8) then frontend part (0/2) on the default branch: the
	// merged total is 80%. The badge and dashboard must show 80%, not the 0%
	// of the last part uploaded.
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main", "part": "backend"}, backendPart)
	doUpload(t, f, "secret-token", map[string]string{"commit": "c1", "branch": "main", "part": "frontend"}, frontendPart)

	req := httptest.NewRequest(http.MethodGet, "/badge/bitbucket/acme/widgets.svg", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if svg := rec.Body.String(); !strings.Contains(svg, ">80.0%<") {
		t.Errorf("badge should show merged 80.0%%, got: %s", svg)
	}

	// The dashboard coverage bar shows the merged 80.0%. (The badge check
	// above already proves the last part's 0% is not what surfaces.)
	if body := get(f, "/").Body.String(); !strings.Contains(body, "80.0%") {
		t.Errorf("dashboard should show merged 80.0%%: %s", body)
	}
}
