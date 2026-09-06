package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/gocov/gocov/internal/store"
)

// The memory store stands in for postgres in handler tests, and postgres
// hands back a fresh slice on every read. A caller that mutates what it
// got back must therefore never reach the stored row.
func TestUsersNeverAliasForgeWorkspaces(t *testing.T) {
	ctx := context.Background()
	s := New()
	u := &store.User{Forge: "github", ForgeUUID: "1", ForgeWorkspaces: []string{"acme"}, ForgeOwnedWorkspaces: []string{"acme"}}
	if err := s.UpsertUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.ForgeWorkspaces[0] = "mutated-after-upsert"

	got, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ForgeWorkspaces, []string{"acme"}) {
		t.Fatalf("stored workspaces follow the caller's slice: %v", got.ForgeWorkspaces)
	}
	got.ForgeWorkspaces[0] = "mutated-after-read"
	got.ForgeOwnedWorkspaces[0] = "mutated-after-read"

	again, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.ForgeWorkspaces, []string{"acme"}) || !reflect.DeepEqual(again.ForgeOwnedWorkspaces, []string{"acme"}) {
		t.Fatalf("read result aliases the store: %v %v", again.ForgeWorkspaces, again.ForgeOwnedWorkspaces)
	}

	// Re-login replaces the snapshot; the caller's new slice must not be
	// adopted either.
	relogin := &store.User{Forge: "github", ForgeUUID: "1", ForgeWorkspaces: []string{"acme", "newco"}}
	if err := s.UpsertUser(ctx, relogin); err != nil {
		t.Fatal(err)
	}
	relogin.ForgeWorkspaces[1] = "mutated"
	final, _ := s.UserByID(ctx, u.ID)
	if !reflect.DeepEqual(final.ForgeWorkspaces, []string{"acme", "newco"}) {
		t.Fatalf("re-login snapshot aliased: %v", final.ForgeWorkspaces)
	}
}
