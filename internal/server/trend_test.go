package server

import (
	"strings"
	"testing"
	"time"

	"github.com/bykclk/gocov/internal/store"
)

// trendUpload builds an upload for newTrendView tests. Callers pass
// uploads newest-first, as ListBranchUploads returns them.
func trendUpload(id int64, pct float64, prID string, gateFailed bool) *store.Upload {
	return &store.Upload{
		ID: id, CommitSHA: strings.Repeat("a", 6) + "sha", Branch: "main",
		PRID: prID, TotalPct: pct, GateFailed: gateFailed,
		CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Hour),
	}
}

func TestTrendViewTooFewUploads(t *testing.T) {
	if v := newTrendView("main", nil); v != nil {
		t.Errorf("no uploads: got %+v, want nil", v)
	}
	if v := newTrendView("main", []*store.Upload{trendUpload(1, 80, "", false)}); v != nil {
		t.Errorf("one upload: got %+v, want nil", v)
	}
	// Two uploads but one is a PR build: one point left, still no chart.
	ups := []*store.Upload{trendUpload(2, 90, "7", false), trendUpload(1, 80, "", false)}
	if v := newTrendView("main", ups); v != nil {
		t.Errorf("one non-PR upload: got %+v, want nil", v)
	}
}

func TestTrendViewExcludesPRUploads(t *testing.T) {
	ups := []*store.Upload{ // newest first
		trendUpload(3, 90, "", false),
		trendUpload(2, 50, "7", false), // PR build, must not appear
		trendUpload(1, 80, "", false),
	}
	v := newTrendView("main", ups)
	if v == nil {
		t.Fatal("nil view")
	}
	if len(v.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(v.Points))
	}
	for _, p := range v.Points {
		if p.ID == 2 {
			t.Errorf("PR upload leaked into the trend: %+v", p)
		}
	}
}

func TestTrendViewPointPlacement(t *testing.T) {
	ups := []*store.Upload{ // newest first: chronological pcts are 70, 75, 80
		trendUpload(3, 80, "", false),
		trendUpload(2, 75, "", false),
		trendUpload(1, 70, "", false),
	}
	v := newTrendView("main", ups)
	if v == nil {
		t.Fatal("nil view")
	}
	// Chronological left to right: oldest upload at the left edge.
	if v.Points[0].ID != 1 || v.Points[2].ID != 3 {
		t.Errorf("point order = %d,%d,%d, want 1,2,3", v.Points[0].ID, v.Points[1].ID, v.Points[2].ID)
	}
	// X spans the plot area evenly: 46, 416, 786.
	for i, want := range []float64{46, 416, 786} {
		if v.Points[i].X != want {
			t.Errorf("point %d X = %g, want %g", i, v.Points[i].X, want)
		}
	}
	// Y: range 70-80 padded by 1 -> 69..81 over 124px below y=12.
	// y(70) = 12 + 124*11/12 = 125.7; y(75) = 74 (middle); y(80) = 22.3.
	for i, want := range []float64{125.7, 74, 22.3} {
		if v.Points[i].Y != want {
			t.Errorf("point %d Y = %g, want %g", i, v.Points[i].Y, want)
		}
	}
	if want := "M46 125.7 L416 74 L786 22.3"; v.Path != want {
		t.Errorf("path = %q, want %q", v.Path, want)
	}
	// Min and max gridlines labeled with the data's extremes.
	if len(v.Grid) != 2 || v.Grid[0].Label != "80.0%" || v.Grid[1].Label != "70.0%" {
		t.Errorf("grid = %+v, want labeled 80.0%% and 70.0%%", v.Grid)
	}
	if v.CurLabel != "80.0%" {
		t.Errorf("current label = %q, want 80.0%%", v.CurLabel)
	}
	if v.FirstDate != "2026-08-01" || v.LastDate != "2026-08-01" {
		t.Errorf("dates = %q..%q", v.FirstDate, v.LastDate)
	}
}

func TestTrendViewFlatSeries(t *testing.T) {
	ups := []*store.Upload{trendUpload(2, 75, "", false), trendUpload(1, 75, "", false)}
	v := newTrendView("main", ups)
	if v == nil {
		t.Fatal("nil view")
	}
	// A flat series pads ±0.5 so the line sits mid-chart, with one gridline.
	if len(v.Grid) != 1 || v.Grid[0].Label != "75.0%" {
		t.Errorf("grid = %+v, want a single 75.0%% line", v.Grid)
	}
	if v.Points[0].Y != v.Points[1].Y {
		t.Errorf("flat series not flat: %g vs %g", v.Points[0].Y, v.Points[1].Y)
	}
}

func TestTrendViewGateMarkers(t *testing.T) {
	ups := []*store.Upload{
		trendUpload(2, 60, "", true),
		trendUpload(1, 80, "", false),
	}
	v := newTrendView("main", ups)
	if v == nil {
		t.Fatal("nil view")
	}
	if v.Points[0].GateFailed || !v.Points[1].GateFailed {
		t.Errorf("gate flags = %v,%v, want false,true", v.Points[0].GateFailed, v.Points[1].GateFailed)
	}
}

func TestTrendViewScaleClampsAt100(t *testing.T) {
	ups := []*store.Upload{trendUpload(2, 100, "", false), trendUpload(1, 99, "", false)}
	v := newTrendView("main", ups)
	if v == nil {
		t.Fatal("nil view")
	}
	// Padding must not push the scale past 100: the 100% point sits at the
	// top gridline, not above it.
	if v.Points[1].Y != v.Grid[0].Y {
		t.Errorf("100%% point Y = %g, top gridline Y = %g", v.Points[1].Y, v.Grid[0].Y)
	}
	if v.Grid[0].Label != "100.0%" {
		t.Errorf("top gridline label = %q", v.Grid[0].Label)
	}
}
