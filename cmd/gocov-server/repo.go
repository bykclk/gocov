package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/bykclk/gocov/internal/blobstore"
	"github.com/bykclk/gocov/internal/store"
)

// errPrinted signals that the error text was already written to the command
// output (the flag package prints parse errors and usage itself), so main
// must not print it again.
var errPrinted = errors.New("error already reported")

const repoUsage = `usage: gocov-server repo <command>

commands:
  add           register a repo and print its upload token
  list          list registered repos
  rotate-token  generate a new upload token (the old one stops working)
  update        change default branch or forge credentials
  remove        delete a repo with all its uploads (requires -force)
`

// repoCmd dispatches the repo admin subcommands. It takes the stores and
// output writer so tests can run it against the in-memory implementations.
func repoCmd(ctx context.Context, st store.Store, blobs blobstore.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", repoUsage)
	}
	switch args[0] {
	case "add":
		return repoAdd(ctx, st, args[1:], out)
	case "list":
		return repoList(ctx, st, args[1:], out)
	case "rotate-token":
		return repoRotateToken(ctx, st, args[1:], out)
	case "update":
		return repoUpdate(ctx, st, args[1:], out)
	case "remove":
		return repoRemove(ctx, st, blobs, args[1:], out)
	default:
		return fmt.Errorf("unknown repo command %q\n%s", args[0], repoUsage)
	}
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// parseFlags parses args. stop means the command must return immediately:
// with a nil error for -h/-help (usage was shown), or with errPrinted for
// parse errors (the flag package already reported them to the output).
// Positional leftovers are rejected: flag parsing stops at the first
// non-flag argument, so "remove -slug x stray -force" would otherwise
// silently drop -force and dry-run with exit code 0.
func parseFlags(fs *flag.FlagSet, args []string) (stop bool, err error) {
	switch err := fs.Parse(args); {
	case err == nil:
		if fs.NArg() > 0 {
			return true, fmt.Errorf("unexpected argument %q (flags must precede it)", fs.Arg(0))
		}
		return false, nil
	case errors.Is(err, flag.ErrHelp):
		return true, nil
	default:
		return true, errPrinted
	}
}

func newToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// gateFlags carries the coverage-gate flags shared by the repo and
// workspace commands. Empty values leave the gate untouched, so update
// commands can change one rule without resetting the others.
type gateFlags struct {
	minCoverage     *string
	minDiffCoverage *string
	maxDrop         *string
}

func addGateFlags(fs *flag.FlagSet) gateFlags {
	return gateFlags{
		minCoverage:     fs.String("min-coverage", "", "gate: minimum total coverage percent, e.g. 80"),
		minDiffCoverage: fs.String("min-diff-coverage", "", "gate: minimum diff coverage percent for PR uploads"),
		maxDrop:         fs.String("max-drop", "", "gate: max allowed total-coverage drop in points; 0 forbids any drop"),
	}
}

// apply parses the provided flags into g; returns whether anything was set.
func (f gateFlags) apply(g *store.Gate) (bool, error) {
	changed := false
	for _, item := range []struct {
		name  string
		value string
		dst   **float64
	}{
		{"min-coverage", *f.minCoverage, &g.MinCoverage},
		{"min-diff-coverage", *f.minDiffCoverage, &g.MinDiffCoverage},
		{"max-drop", *f.maxDrop, &g.MaxCoverageDrop},
	} {
		if item.value == "" {
			continue
		}
		v, err := strconv.ParseFloat(item.value, 64)
		if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
			return false, fmt.Errorf("-%s must be a percentage between 0 and 100", item.name)
		}
		*item.dst = &v
		changed = true
	}
	return changed, nil
}

// gateSummary renders a gate for list output, e.g. "total>=80% drop<=0.5%".
func gateSummary(g store.Gate) string {
	var parts []string
	if g.MinCoverage != nil {
		parts = append(parts, fmt.Sprintf("total>=%.4g%%", *g.MinCoverage))
	}
	if g.MinDiffCoverage != nil {
		parts = append(parts, fmt.Sprintf("diff>=%.4g%%", *g.MinDiffCoverage))
	}
	if g.MaxCoverageDrop != nil {
		parts = append(parts, fmt.Sprintf("drop<=%.4g%%", *g.MaxCoverageDrop))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// forgeCredFlags validates the credential flags and returns the
// credentials map, or nil when none were set. Bitbucket's flag pair and
// GitHub's token are mutually exclusive — a repo lives on one forge.
func forgeCredFlags(bbUsername, bbPassword, ghToken string) (map[string]string, error) {
	if bbUsername == "" && bbPassword == "" {
		if ghToken == "" {
			return nil, nil
		}
		return map[string]string{"token": ghToken}, nil
	}
	if ghToken != "" {
		return nil, fmt.Errorf("-gh-token cannot be combined with -bb-username/-bb-app-password")
	}
	if bbUsername == "" || bbPassword == "" {
		return nil, fmt.Errorf("-bb-username and -bb-app-password must be set together")
	}
	return map[string]string{"username": bbUsername, "app_password": bbPassword}, nil
}

func repoAdd(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo add", out)
	slug := fs.String("slug", "", "repo slug, namespaced: workspace/repo (required)")
	forgeName := fs.String("forge", "bitbucket", "forge hosting the repo")
	defaultBranch := fs.String("default-branch", "main", "default branch")
	bbUser := fs.String("bb-username", "", "Bitbucket username for build status pushes (optional)")
	bbPassword := fs.String("bb-app-password", "", "Bitbucket app password (optional)")
	ghToken := fs.String("gh-token", "", "GitHub access token for -forge github repos (optional)")
	gf := addGateFlags(fs)
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	creds, err := forgeCredFlags(*bbUser, *bbPassword, *ghToken)
	if err != nil {
		return err
	}
	var gate store.Gate
	if _, err := gf.apply(&gate); err != nil {
		return err
	}
	token, err := newToken()
	if err != nil {
		return err
	}

	r := &store.Repo{
		Forge:            *forgeName,
		Slug:             *slug,
		Token:            token,
		DefaultBranch:    *defaultBranch,
		ForgeCredentials: creds,
		Gate:             gate,
	}
	if err := st.CreateRepo(ctx, r); err != nil {
		return fmt.Errorf("creating repo: %w", err)
	}
	fmt.Fprintf(out, "repo %s added\nupload token: %s\n", r.Slug, r.Token)
	if prefix, _, ok := strings.Cut(*slug, "/"); ok {
		if err := ensureWorkspace(ctx, st, prefix, *forgeName, *defaultBranch, out); err != nil {
			return err
		}
	}
	return nil
}

// ensureWorkspace creates the workspace row for a slug prefix when none
// exists yet (M2/D6: every repo belongs to a workspace). It is idempotent —
// an existing prefix is left untouched — and prints the generated token so
// the operator can wire up prefix-wide auto-registration.
func ensureWorkspace(ctx context.Context, st store.Store, prefix, forge, defaultBranch string, out io.Writer) error {
	if _, err := st.WorkspaceByPrefix(ctx, prefix); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("looking up workspace: %w", err)
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	w := &store.Workspace{
		Forge:         forge,
		Prefix:        prefix,
		Token:         token,
		DefaultBranch: defaultBranch,
	}
	if err := st.CreateWorkspace(ctx, w); err != nil {
		return fmt.Errorf("creating workspace: %w", err)
	}
	fmt.Fprintf(out, "workspace %s created\nworkspace upload token: %s\n", w.Prefix, w.Token)
	return nil
}

func repoList(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo list", out)
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Fprintln(out, "no repos registered")
		return nil
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tFORGE\tDEFAULT BRANCH\tCREDENTIALS\tGATE\tCREATED")
	for _, r := range repos {
		creds := "-"
		if len(r.ForgeCredentials) > 0 {
			creds = "set"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Slug, r.Forge, r.DefaultBranch, creds, gateSummary(r.Gate), r.CreatedAt.Format("2006-01-02"))
	}
	return tw.Flush()
}

func repoRotateToken(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo rotate-token", out)
	slug := fs.String("slug", "", "repo slug (required)")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	r, err := st.RepoBySlug(ctx, *slug)
	if err != nil {
		return fmt.Errorf("loading repo %s: %w", *slug, err)
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	r.Token = token
	if err := st.UpdateRepo(ctx, r); err != nil {
		return fmt.Errorf("updating repo %s: %w", *slug, err)
	}
	fmt.Fprintf(out, "token rotated for %s\nnew upload token: %s\n", r.Slug, r.Token)
	fmt.Fprintln(out, "the previous token no longer works; update CI configuration")
	return nil
}

func repoUpdate(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo update", out)
	slug := fs.String("slug", "", "repo slug (required)")
	defaultBranch := fs.String("default-branch", "", "new default branch")
	bbUser := fs.String("bb-username", "", "Bitbucket username")
	bbPassword := fs.String("bb-app-password", "", "Bitbucket app password")
	ghToken := fs.String("gh-token", "", "GitHub access token")
	clearCreds := fs.Bool("clear-credentials", false, "remove stored forge credentials")
	gf := addGateFlags(fs)
	clearGate := fs.Bool("clear-gate", false, "remove all coverage gate rules")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	creds, err := forgeCredFlags(*bbUser, *bbPassword, *ghToken)
	if err != nil {
		return err
	}
	if *clearCreds && creds != nil {
		return fmt.Errorf("-clear-credentials cannot be combined with credential flags")
	}

	r, err := st.RepoBySlug(ctx, *slug)
	if err != nil {
		return fmt.Errorf("loading repo %s: %w", *slug, err)
	}
	gateChanged, err := gf.apply(&r.Gate)
	if err != nil {
		return err
	}
	if *clearGate && gateChanged {
		return fmt.Errorf("-clear-gate cannot be combined with gate flags")
	}
	if *defaultBranch == "" && creds == nil && !*clearCreds && !gateChanged && !*clearGate {
		return fmt.Errorf("nothing to update: pass -default-branch, credential or gate flags")
	}
	if *defaultBranch != "" {
		r.DefaultBranch = *defaultBranch
	}
	if creds != nil {
		r.ForgeCredentials = creds
	}
	if *clearCreds {
		r.ForgeCredentials = nil
	}
	if *clearGate {
		r.Gate = store.Gate{}
	}
	if err := st.UpdateRepo(ctx, r); err != nil {
		return fmt.Errorf("updating repo %s: %w", *slug, err)
	}
	fmt.Fprintf(out, "repo %s updated\n", r.Slug)
	return nil
}

func repoRemove(ctx context.Context, st store.Store, blobs blobstore.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo remove", out)
	slug := fs.String("slug", "", "repo slug (required)")
	force := fs.Bool("force", false, "actually delete; without it only a summary is printed")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	r, err := st.RepoBySlug(ctx, *slug)
	if err != nil {
		return fmt.Errorf("loading repo %s: %w", *slug, err)
	}
	// Uploads landing between this snapshot and DeleteRepo leave their
	// blobs orphaned — a harmless, milliseconds-wide window; prefer
	// removing repos while their CI is quiet.
	uploads, err := st.ListUploads(ctx, r.ID, 0)
	if err != nil {
		return fmt.Errorf("listing uploads: %w", err)
	}

	if !*force {
		fmt.Fprintf(out, "would remove repo %s with %d upload(s) and their raw profiles\n", r.Slug, len(uploads))
		fmt.Fprintln(out, "re-run with -force to delete permanently")
		return nil
	}

	// Blob keys must be collected before the upload rows disappear, but the
	// blobs themselves are deleted after the repo: if DeleteRepo fails the
	// repo stays fully intact, whereas a failed blob delete only leaves
	// dead weight behind.
	keys := make([]string, 0, len(uploads))
	for _, u := range uploads {
		if u.RawBlobKey != "" {
			keys = append(keys, u.RawBlobKey)
		}
	}
	if err := st.DeleteRepo(ctx, r.ID); err != nil {
		return fmt.Errorf("deleting repo %s: %w", *slug, err)
	}
	blobErrs := 0
	for _, key := range keys {
		if err := blobs.Delete(ctx, key); err != nil {
			blobErrs++
			fmt.Fprintf(out, "warning: orphaned blob %s: %v\n", key, err)
		}
	}
	fmt.Fprintf(out, "repo %s removed (%d uploads", r.Slug, len(uploads))
	if blobErrs > 0 {
		fmt.Fprintf(out, ", %d blob(s) could not be deleted", blobErrs)
	}
	fmt.Fprintln(out, ")")
	return nil
}
