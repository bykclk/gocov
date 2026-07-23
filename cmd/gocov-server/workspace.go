package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/bykclk/gocov/internal/store"
)

const workspaceUsage = `usage: gocov-server workspace <command>

A workspace token authorizes uploads for every repo under a slug prefix
("workspace" in "workspace/repo"); unknown repos are registered on their
first upload. Set it once as a Bitbucket workspace variable instead of
managing per-repo tokens.

commands:
  add           register a workspace prefix and print its upload token
  list          list workspaces
  rotate-token  generate a new workspace token (the old one stops working)
  update        change the default branch for auto-registered repos
  remove        delete the workspace token (repos are kept; requires -force)
`

// workspaceCmd dispatches the workspace admin subcommands.
func workspaceCmd(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", workspaceUsage)
	}
	switch args[0] {
	case "add":
		return workspaceAdd(ctx, st, args[1:], out)
	case "list":
		return workspaceList(ctx, st, args[1:], out)
	case "rotate-token":
		return workspaceRotateToken(ctx, st, args[1:], out)
	case "update":
		return workspaceUpdate(ctx, st, args[1:], out)
	case "remove":
		return workspaceRemove(ctx, st, args[1:], out)
	default:
		return fmt.Errorf("unknown workspace command %q\n%s", args[0], workspaceUsage)
	}
}

func workspaceAdd(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("workspace add", out)
	prefix := fs.String("prefix", "", "workspace part of repo slugs, e.g. myworkspace (required)")
	forgeName := fs.String("forge", "bitbucket", "forge for auto-registered repos")
	defaultBranch := fs.String("default-branch", "main", "default branch for auto-registered repos when the forge cannot be asked")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("-prefix is required")
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	w := &store.Workspace{
		Forge:         *forgeName,
		Prefix:        *prefix,
		Token:         token,
		DefaultBranch: *defaultBranch,
	}
	if err := st.CreateWorkspace(ctx, w); err != nil {
		return fmt.Errorf("creating workspace: %w", err)
	}
	fmt.Fprintf(out, "workspace %s added\nupload token: %s\n", w.Prefix, w.Token)
	fmt.Fprintf(out, "set it once as a %s workspace variable; repos under %s/ register themselves on first upload\n",
		w.Forge, w.Prefix)
	return nil
}

func workspaceList(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("workspace list", out)
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	workspaces, err := st.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	if len(workspaces) == 0 {
		fmt.Fprintln(out, "no workspaces registered")
		return nil
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PREFIX\tFORGE\tDEFAULT BRANCH\tCREATED")
	for _, w := range workspaces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			w.Prefix, w.Forge, w.DefaultBranch, w.CreatedAt.Format("2006-01-02"))
	}
	return tw.Flush()
}

func workspaceRotateToken(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("workspace rotate-token", out)
	prefix := fs.String("prefix", "", "workspace prefix (required)")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("-prefix is required")
	}
	w, err := st.WorkspaceByPrefix(ctx, *prefix)
	if err != nil {
		return fmt.Errorf("loading workspace %s: %w", *prefix, err)
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	w.Token = token
	if err := st.UpdateWorkspace(ctx, w); err != nil {
		return fmt.Errorf("updating workspace %s: %w", *prefix, err)
	}
	fmt.Fprintf(out, "token rotated for workspace %s\nnew upload token: %s\n", w.Prefix, w.Token)
	fmt.Fprintln(out, "the previous token no longer works; update the workspace variable")
	return nil
}

func workspaceUpdate(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("workspace update", out)
	prefix := fs.String("prefix", "", "workspace prefix (required)")
	defaultBranch := fs.String("default-branch", "", "new default branch for auto-registered repos")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("-prefix is required")
	}
	if *defaultBranch == "" {
		return fmt.Errorf("nothing to update: pass -default-branch")
	}
	w, err := st.WorkspaceByPrefix(ctx, *prefix)
	if err != nil {
		return fmt.Errorf("loading workspace %s: %w", *prefix, err)
	}
	w.DefaultBranch = *defaultBranch
	if err := st.UpdateWorkspace(ctx, w); err != nil {
		return fmt.Errorf("updating workspace %s: %w", *prefix, err)
	}
	fmt.Fprintf(out, "workspace %s updated\n", w.Prefix)
	return nil
}

func workspaceRemove(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("workspace remove", out)
	prefix := fs.String("prefix", "", "workspace prefix (required)")
	force := fs.Bool("force", false, "actually delete; without it only a summary is printed")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("-prefix is required")
	}
	w, err := st.WorkspaceByPrefix(ctx, *prefix)
	if err != nil {
		return fmt.Errorf("loading workspace %s: %w", *prefix, err)
	}
	if !*force {
		fmt.Fprintf(out, "would remove workspace %s: its token stops working, already-registered repos are kept\n", w.Prefix)
		fmt.Fprintln(out, "re-run with -force to delete")
		return nil
	}
	if err := st.DeleteWorkspace(ctx, w.ID); err != nil {
		return fmt.Errorf("deleting workspace %s: %w", *prefix, err)
	}
	fmt.Fprintf(out, "workspace %s removed; existing repos and their tokens are untouched\n", w.Prefix)
	return nil
}
