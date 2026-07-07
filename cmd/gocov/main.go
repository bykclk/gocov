// gocov is the coverage uploader CLI, run inside CI:
//
//	gocov upload [flags] coverage.out
//
// Repo, commit, branch and PR id are auto-detected from Bitbucket Pipelines
// environment variables, falling back to git. Server and token come from
// GOCOV_SERVER / GOCOV_TOKEN or flags.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gocov:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "upload" {
		return fmt.Errorf("usage: gocov upload [flags] <profile file>")
	}

	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	server := fs.String("server", os.Getenv("GOCOV_SERVER"), "gocov server URL (or $GOCOV_SERVER)")
	token := fs.String("token", os.Getenv("GOCOV_TOKEN"), "per-repo upload token (or $GOCOV_TOKEN)")
	repo := fs.String("repo", "", "repo slug workspace/repo (default: auto-detect)")
	commit := fs.String("commit", "", "commit SHA (default: auto-detect)")
	branch := fs.String("branch", "", "branch name (default: auto-detect)")
	pr := fs.String("pr", "", "pull request id (default: auto-detect)")
	format := fs.String("format", "go", "coverage profile format")
	pathPrefix := fs.String("path-prefix", "", "prefix mapping profile paths to repo paths, e.g. the Go module path (default: from go.mod)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: gocov upload [flags] <profile file>")
	}
	profilePath := fs.Arg(0)

	if *server == "" {
		return fmt.Errorf("server URL required: set -server or $GOCOV_SERVER")
	}
	if *token == "" {
		return fmt.Errorf("upload token required: set -token or $GOCOV_TOKEN")
	}

	build := detectBuild(osEnv, runGit)
	build.merge(buildInfo{Repo: *repo, Commit: *commit, Branch: *branch, PRID: *pr})
	if build.Commit == "" {
		return fmt.Errorf("could not detect commit SHA: pass -commit")
	}
	prefix := *pathPrefix
	if prefix == "" && *format == "go" {
		prefix = moduleFromGoMod("go.mod")
	}

	resp, err := upload(uploadRequest{
		Server:      *server,
		Token:       *token,
		Format:      *format,
		PathPrefix:  prefix,
		ProfilePath: profilePath,
		Build:       build,
	})
	if err != nil {
		return err
	}

	fmt.Printf("uploaded: %.1f%% (%d/%d statements)", resp.TotalPct, resp.CoveredStmts, resp.TotalStmts)
	if resp.DeltaPct != nil {
		fmt.Printf(", delta %+.1f%%", *resp.DeltaPct)
	}
	fmt.Println()
	if resp.DiffPct != nil && resp.DiffCoveredLines != nil && resp.DiffTotalLines != nil {
		fmt.Printf("diff coverage: %.1f%% (%d/%d changed lines)\n",
			*resp.DiffPct, *resp.DiffCoveredLines, *resp.DiffTotalLines)
	} else if resp.DiffStatus != "" {
		fmt.Printf("diff coverage: %s\n", resp.DiffStatus)
	}
	fmt.Printf("build status: %s\n", resp.BuildStatus)
	if resp.PRComment != "" {
		fmt.Printf("pr comment: %s\n", resp.PRComment)
	}
	return nil
}

func osEnv(key string) string { return os.Getenv(key) }
