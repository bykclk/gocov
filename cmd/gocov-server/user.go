package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/bykclk/gocov/internal/store"
)

const userUsage = `usage: gocov-server user <command>

commands:
  list    list web UI users (JIT-provisioned on first sign-in)
  remove  delete a user and their sessions; they can sign in again while
          still a workspace member
`

// userCmd dispatches the web UI user admin subcommands.
func userCmd(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", userUsage)
	}
	switch args[0] {
	case "list":
		return userList(ctx, st, args[1:], out)
	case "remove":
		return userRemove(ctx, st, args[1:], out)
	default:
		return fmt.Errorf("unknown user command %q\n%s", args[0], userUsage)
	}
}

func userList(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("user list", out)
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Fprintln(out, "no users; accounts are created on first sign-in")
		return nil
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tEMAIL\tFORGE\tFIRST LOGIN\tLAST LOGIN")
	for _, u := range users {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			u.DisplayName, u.Email, u.Forge,
			u.CreatedAt.Format("2006-01-02"), u.LastLoginAt.Format("2006-01-02"))
	}
	return tw.Flush()
}

func userRemove(ctx context.Context, st store.Store, args []string, out io.Writer) error {
	fs := newFlagSet("user remove", out)
	email := fs.String("email", "", "email of the user to remove (required)")
	if stop, err := parseFlags(fs, args); stop {
		return err
	}
	if *email == "" {
		return fmt.Errorf("-email is required")
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return err
	}
	removed := 0
	for _, u := range users {
		if u.Email != *email {
			continue
		}
		// DeleteUser takes the sessions with it: access dies immediately.
		if err := st.DeleteUser(ctx, u.ID); err != nil {
			return fmt.Errorf("removing user %s: %w", u.Email, err)
		}
		removed++
	}
	if removed == 0 {
		return fmt.Errorf("no user with email %s", *email)
	}
	fmt.Fprintf(out, "removed %d user(s) with email %s; their sessions are revoked\n", removed, *email)
	fmt.Fprintln(out, "signing in again re-creates the account while they remain a workspace member")
	return nil
}
