package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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

// bbCreds validates the Bitbucket credential flag pair and returns the
// credentials map, or nil when both flags are empty.
func bbCreds(username, password string) (map[string]string, error) {
	if username == "" && password == "" {
		return nil, nil
	}
	if username == "" || password == "" {
		return nil, fmt.Errorf("-bb-username and -bb-app-password must be set together")
	}
	return map[string]string{"username": username, "app_password": password}, nil
}

func repoAdd(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("repo add", out)
	slug := fs.String("slug", "", "repo slug, namespaced: workspace/repo (required)")
	forgeName := fs.String("forge", "bitbucket", "forge hosting the repo")
	defaultBranch := fs.String("default-branch", "main", "default branch")
	bbUser := fs.String("bb-username", "", "Bitbucket username for build status pushes (optional)")
	bbPassword := fs.String("bb-app-password", "", "Bitbucket app password (optional)")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	creds, err := bbCreds(*bbUser, *bbPassword)
	if err != nil {
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
	}
	if err := st.CreateRepo(ctx, r); err != nil {
		return fmt.Errorf("creating repo: %w", err)
	}
	fmt.Fprintf(out, "repo %s added\nupload token: %s\n", r.Slug, r.Token)
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
	fmt.Fprintln(tw, "SLUG\tFORGE\tDEFAULT BRANCH\tCREDENTIALS\tCREATED")
	for _, r := range repos {
		creds := "-"
		if len(r.ForgeCredentials) > 0 {
			creds = "set"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Slug, r.Forge, r.DefaultBranch, creds, r.CreatedAt.Format("2006-01-02"))
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
	clearCreds := fs.Bool("clear-credentials", false, "remove stored forge credentials")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("-slug is required")
	}
	creds, err := bbCreds(*bbUser, *bbPassword)
	if err != nil {
		return err
	}
	if *clearCreds && creds != nil {
		return fmt.Errorf("-clear-credentials cannot be combined with -bb-username/-bb-app-password")
	}
	if *defaultBranch == "" && creds == nil && !*clearCreds {
		return fmt.Errorf("nothing to update: pass -default-branch, -bb-username/-bb-app-password or -clear-credentials")
	}

	r, err := st.RepoBySlug(ctx, *slug)
	if err != nil {
		return fmt.Errorf("loading repo %s: %w", *slug, err)
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
