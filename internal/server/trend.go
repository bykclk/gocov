package server

import (
	"fmt"
	"math"
	"strings"

	"github.com/bykclk/gocov/internal/store"
)

// The trend chart plots total coverage over the branch's recent uploads
// as server-rendered inline SVG. Geometry lives here so the template only
// places precomputed coordinates.
const (
	trendUploadLimit = 60

	trendW, trendH       = 800, 160
	trendPadL, trendPadR = 46, 14
	trendPadT, trendPadB = 12, 24
)

// trendPoint is one upload's marker on the chart.
type trendPoint struct {
	X, Y       float64
	ID         int64
	GateFailed bool
	Title      string // hover text: "2026-08-05 · 82.3% · abc123def456"
}

// trendGridLine is a labeled horizontal gridline (the series min and max).
type trendGridLine struct {
	Y     float64
	Label string
}

// trendView is the fully computed chart handed to the repo template.
type trendView struct {
	W, H       int
	X0, X1     int // plot's left/right edge; gridlines and axis labels hang off these
	Branch     string
	Path       string // the polyline through every point
	Points     []trendPoint
	Grid       []trendGridLine
	CurLabel   string // current (latest) percentage, drawn at the last point
	CurX, CurY float64
	FirstDate  string
	LastDate   string
}

// round1 keeps SVG coordinates at one decimal so output stays compact and
// deterministic.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// newTrendView builds the chart for a branch from its uploads, given
// newest-first as ListBranchUploads returns them. PR uploads are excluded:
// the branch trend reflects the branch's own uploads. Returns nil when
// fewer than two points remain — the page then omits the section.
func newTrendView(branch string, ups []*store.Upload) *trendView {
	var series []*store.Upload // chronological
	for i := len(ups) - 1; i >= 0; i-- {
		if ups[i].PRID == "" {
			series = append(series, ups[i])
		}
	}
	if len(series) < 2 {
		return nil
	}

	// Y auto-scales to the data's range with padding, not 0-100: a 72-78%
	// story must not flatline. Clamped so the padding never invents
	// impossible percentages.
	lo, hi := series[0].TotalPct, series[0].TotalPct
	for _, u := range series[1:] {
		lo = math.Min(lo, u.TotalPct)
		hi = math.Max(hi, u.TotalPct)
	}
	pad := math.Max((hi-lo)*0.1, 0.5)
	yLo := math.Max(0, lo-pad)
	yHi := math.Min(100, hi+pad)

	plotW := float64(trendW - trendPadL - trendPadR)
	plotH := float64(trendH - trendPadT - trendPadB)
	x := func(i int) float64 {
		return round1(trendPadL + plotW*float64(i)/float64(len(series)-1))
	}
	y := func(pct float64) float64 {
		return round1(trendPadT + plotH*(yHi-pct)/(yHi-yLo))
	}

	v := &trendView{
		W:         trendW,
		H:         trendH,
		X0:        trendPadL,
		X1:        trendW - trendPadR,
		Branch:    branch,
		FirstDate: series[0].CreatedAt.Format("2006-01-02"),
		LastDate:  series[len(series)-1].CreatedAt.Format("2006-01-02"),
	}
	var path strings.Builder
	for i, u := range series {
		px, py := x(i), y(u.TotalPct)
		if i == 0 {
			fmt.Fprintf(&path, "M%g %g", px, py)
		} else {
			fmt.Fprintf(&path, " L%g %g", px, py)
		}
		sha := u.CommitSHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		v.Points = append(v.Points, trendPoint{
			X: px, Y: py, ID: u.ID, GateFailed: u.GateFailed,
			Title: fmt.Sprintf("%s · %.1f%% · %s", u.CreatedAt.Format("2006-01-02"), u.TotalPct, sha),
		})
	}
	v.Path = path.String()

	v.Grid = []trendGridLine{{Y: y(hi), Label: fmt.Sprintf("%.1f%%", hi)}}
	if lo != hi {
		v.Grid = append(v.Grid, trendGridLine{Y: y(lo), Label: fmt.Sprintf("%.1f%%", lo)})
	}

	last := v.Points[len(v.Points)-1]
	cur := series[len(series)-1]
	v.CurLabel = fmt.Sprintf("%.1f%%", cur.TotalPct)
	v.CurX = last.X
	v.CurY = last.Y - 9
	if v.CurY < 11 { // too close to the top edge: drop below the point
		v.CurY = last.Y + 18
	}
	return v
}
