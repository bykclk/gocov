// The coverage gate as an uploader meets it: POST a profile, read the
// verdict off the response and off what the forge was told. The gate's
// own rules are unit-tested in internal/core (gate_test.go); these are
// the endpoint's tests, which is why they are named for upload.go rather
// than for a file of their own.

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	blobmem "github.com/gocov/gocov/internal/blobstore/memory"
	"github.com/gocov/gocov/internal/forge"
	"github.com/gocov/gocov/internal/profile"
	"github.com/gocov/gocov/internal/store"
	storemem "github.com/gocov/gocov/internal/store/memory"
)

func TestCoverageGate(t *testing.T) {
	// testProfile is 80% overall.
	setGate := func(t *testing.T, f *fixture, gate store.Gate) {
		t.Helper()
		f.repo.Gate = gate
		if err := f.store.UpdateRepo(t.Context(), f.repo); err != nil {
			t.Fatal(err)
		}
	}
	upload := func(t *testing.T, f *fixture, fields map[string]string, prof string) uploadResponse {
		t.Helper()
		rec := doUpload(t, f, "secret-token", fields, prof)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		var resp uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("no gate configured omits the field", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		resp := upload(t, f, map[string]string{"commit": "c"}, testProfile)
		if resp.Gate != "" {
			t.Errorf("gate = %q, want omitted", resp.Gate)
		}
		if got := f.forge.StatusCalls[0].Status.State; got != forge.StateSuccessful {
			t.Errorf("state = %q", got)
		}
	})

	t.Run("min coverage pass and fail", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MinCoverage: new(float64(75))})
		if resp := upload(t, f, map[string]string{"commit": "c1"}, testProfile); resp.Gate != "passed" {
			t.Errorf("gate = %q, want passed (80%% >= 75%%)", resp.Gate)
		}

		setGate(t, f, store.Gate{MinCoverage: new(float64(90))})
		resp := upload(t, f, map[string]string{"commit": "c2"}, testProfile)
		if !strings.HasPrefix(resp.Gate, "failed: total coverage 80% is below the minimum 90%") {
			t.Errorf("gate = %q", resp.Gate)
		}
		// Build status turns FAILED and carries the reason.
		last := f.forge.StatusCalls[len(f.forge.StatusCalls)-1]
		if last.Status.State != forge.StateFailed {
			t.Errorf("state = %q, want failed", last.Status.State)
		}
		if !strings.Contains(last.Status.Description, "below the minimum") {
			t.Errorf("description = %q", last.Status.Description)
		}
	})

	t.Run("max drop", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MaxCoverageDrop: new(float64(1))})
		upload(t, f, map[string]string{"commit": "c1", "branch": "main"}, testProfile) // 80%

		worse := "mode: set\nexample.com/m/a.go:1.1,5.2 1 1\nexample.com/m/a.go:6.1,7.2 1 0\n" // 50%
		resp := upload(t, f, map[string]string{"commit": "c2", "branch": "main"}, worse)
		if !strings.HasPrefix(resp.Gate, "failed: coverage dropped 30% (allowed 1%)") {
			t.Errorf("gate = %q", resp.Gate)
		}

		// Re-running CI must not launder the failure: the failed upload is
		// not a baseline, so the same profile still drops 30% vs c1.
		resp = upload(t, f, map[string]string{"commit": "c2", "branch": "main"}, worse)
		if !strings.HasPrefix(resp.Gate, "failed: coverage dropped 30%") {
			t.Errorf("gate after re-run = %q, want still failed (no baseline laundering)", resp.Gate)
		}

		// A drop within tolerance passes (still measured against c1).
		setGate(t, f, store.Gate{MaxCoverageDrop: new(float64(50))})
		if resp := upload(t, f, map[string]string{"commit": "c3", "branch": "main"}, worse); resp.Gate != "passed" {
			t.Errorf("gate = %q, want passed within tolerance", resp.Gate)
		}
	})

	t.Run("drop rule cannot be ratcheted on a feature branch", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MaxCoverageDrop: new(float64(25))})
		upload(t, f, map[string]string{"commit": "m1", "branch": "main"}, testProfile) // 80%

		// First branch push: 60%, drop 20 vs main — within tolerance.
		drop20 := "mode: set\nexample.com/m/a.go:1.1,5.2 3 1\nexample.com/m/a.go:6.1,7.2 2 0\n"
		if resp := upload(t, f, map[string]string{"commit": "b1", "branch": "feat"}, drop20); resp.Gate != "passed" {
			t.Fatalf("gate = %q, want passed (20 <= 25)", resp.Gate)
		}
		// Second push drops further to 50%. Vs its own branch that is only
		// -10, but the rule compares against main: -30 fails.
		drop30 := "mode: set\nexample.com/m/a.go:1.1,5.2 1 1\nexample.com/m/a.go:6.1,7.2 1 0\n"
		resp := upload(t, f, map[string]string{"commit": "b2", "branch": "feat"}, drop30)
		if !strings.HasPrefix(resp.Gate, "failed: coverage dropped 30%") {
			t.Errorf("gate = %q, want failed vs default branch baseline", resp.Gate)
		}
	})

	t.Run("coverage exactly at the minimum passes", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MinCoverage: new(float64(57))})
		// 57 of 100 statements: float division yields 56.999999999999993.
		exact := "mode: set\nexample.com/m/a.go:1.1,5.2 57 1\nexample.com/m/a.go:6.1,7.2 43 0\n"
		if resp := upload(t, f, map[string]string{"commit": "c1"}, exact); resp.Gate != "passed" {
			t.Errorf("gate = %q, want passed at exact threshold", resp.Gate)
		}
	})

	t.Run("min diff coverage on PR uploads", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MinDiffCoverage: new(float64(90))})
		f.forge.DiffText = testPRDiff // 2/3 changed lines covered = 66.7%
		resp := upload(t, f, map[string]string{"commit": "c1", "branch": "f", "pr_id": "3"}, testProfile)
		if !strings.HasPrefix(resp.Gate, "failed: diff coverage 66.67% is below the minimum 90%") {
			t.Errorf("gate = %q", resp.Gate)
		}
		// The PR comment carries the gate verdict.
		body := f.forge.CommentCalls[len(f.forge.CommentCalls)-1].Body
		if !strings.Contains(body, "Gate: ❌") {
			t.Errorf("comment body: %q", body)
		}
	})

	t.Run("diff rule is fail-open without diff data", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		setGate(t, f, store.Gate{MinDiffCoverage: new(float64(90))})
		// No DiffText: the fake forge reports diff as not supported.
		resp := upload(t, f, map[string]string{"commit": "c1", "pr_id": "3"}, testProfile)
		if resp.Gate != "passed" {
			t.Errorf("gate = %q, want passed (fail-open)", resp.Gate)
		}
		// Non-PR uploads ignore the diff rule entirely.
		if resp := upload(t, f, map[string]string{"commit": "c2"}, testProfile); resp.Gate != "passed" {
			t.Errorf("gate = %q, want passed for non-PR upload", resp.Gate)
		}
	})

	t.Run("multiple failures reported together", func(t *testing.T) {
		f := newFixture(t, map[string]string{"username": "u", "app_password": "p"})
		// Establish a passing 80% baseline before the gate exists, then
		// tighten the gate: the next upload violates both rules.
		upload(t, f, map[string]string{"commit": "c1", "branch": "main"}, testProfile)
		setGate(t, f, store.Gate{MinCoverage: new(float64(90)), MaxCoverageDrop: new(float64(0))})
		worse := "mode: set\nexample.com/m/a.go:1.1,5.2 1 1\nexample.com/m/a.go:6.1,7.2 1 0\n"
		resp := upload(t, f, map[string]string{"commit": "c2", "branch": "main"}, worse)
		if !strings.Contains(resp.Gate, "below the minimum") || !strings.Contains(resp.Gate, "dropped") {
			t.Errorf("gate = %q, want both failures", resp.Gate)
		}
	})
}

func TestWorkspaceGateInheritedByAutoCreatedRepos(t *testing.T) {
	ctx := t.Context()
	st := storemem.New()
	ws := &store.Workspace{
		Forge: "bitbucket", Prefix: "acme", Token: "ws-token", DefaultBranch: "main",
		Gate: store.Gate{MinCoverage: new(float64(85))},
	}
	if err := st.CreateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Store:   st,
		Blobs:   blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
	})
	f := &fixture{srv: srv, store: st}
	rec := doUpload(t, f, "ws-token", map[string]string{"repo": "acme/newrepo", "commit": "c"}, testProfile)
	var resp uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Gate, "failed: total coverage 80% is below the minimum 85%") {
		t.Errorf("gate = %q, want inherited workspace gate failure", resp.Gate)
	}
	repo, err := st.RepoBySlug(ctx, "bitbucket", "acme/newrepo")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Gate.MinCoverage == nil || *repo.Gate.MinCoverage != 85 {
		t.Errorf("repo gate = %+v, want inherited min 85", repo.Gate)
	}
}
