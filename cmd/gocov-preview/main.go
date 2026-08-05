// Command gocov-preview is a throwaway dev harness: it serves the web UI
// from an in-memory store seeded with a synthetic upload history, for
// eyeballing UI changes without Postgres. Not part of the product.
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"time"

	blobmem "github.com/bykclk/gocov/internal/blobstore/memory"
	"github.com/bykclk/gocov/internal/forge"
	forgefake "github.com/bykclk/gocov/internal/forge/fake"
	"github.com/bykclk/gocov/internal/profile"
	"github.com/bykclk/gocov/internal/server"
	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

func main() {
	ctx := context.Background()
	st := storemem.New()
	repo := &store.Repo{
		Forge: "bitbucket", Slug: "acme/widgets", Token: "tok",
		DefaultBranch: "main", Gate: store.Gate{MinCoverage: pctPtr(70)},
	}
	if err := st.CreateRepo(ctx, repo); err != nil {
		log.Fatal(err)
	}

	// ~45 uploads drifting between ~68% and ~85%, a few gate failures,
	// a couple of PR uploads that must not appear in the trend.
	rnd := rand.New(rand.NewSource(42))
	base := time.Now().Add(-45 * 24 * time.Hour)
	pct := 74.0
	for i := 0; i < 45; i++ {
		pct += rnd.Float64()*4 - 2 + 0.1*math.Sin(float64(i)/4)
		pct = math.Max(66, math.Min(88, pct))
		u := &store.Upload{
			RepoID:    repo.ID,
			CommitSHA: fmt.Sprintf("%040x", i),
			Branch:    "main",
			Format:    "go",
			TotalPct:  pct, CoveredStmts: int64(pct * 10), TotalStmts: 1000,
			GateFailed: pct < 70,
			CreatedAt:  base.Add(time.Duration(i) * 24 * time.Hour),
		}
		if i%15 == 7 {
			u.PRID = "9"
			u.TotalPct = 20 // would be an obvious outlier if it leaked in
		}
		if err := st.CreateUpload(ctx, u, nil); err != nil {
			log.Fatal(err)
		}
	}

	srv := server.New(server.Config{
		Store: st, Blobs: blobmem.New(),
		Parsers: map[string]profile.Parser{"go": profile.GoParser{}},
		Forges:  map[string]forge.Factory{"bitbucket": forgefake.New().Factory()},
		BaseURL: "http://localhost:8099",
	})
	log.Println("preview on :8099")
	log.Fatal(http.ListenAndServe(":8099", srv))
}

func pctPtr(v float64) *float64 { return &v }
