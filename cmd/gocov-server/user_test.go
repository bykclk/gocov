package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bykclk/gocov/internal/store"
	storemem "github.com/bykclk/gocov/internal/store/memory"
)

func runUserCmd(t *testing.T, st store.Store, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := userCmd(context.Background(), st, args, &out)
	return out.String(), err
}

func addUser(t *testing.T, st store.Store, uuid, name, email string) *store.User {
	t.Helper()
	u := &store.User{Forge: "bitbucket", ForgeUUID: uuid, DisplayName: name, Email: email}
	if err := st.UpsertUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserList(t *testing.T) {
	st := storemem.New()

	out, err := runUserCmd(t, st, "list")
	if err != nil || !strings.Contains(out, "no users") {
		t.Errorf("empty list: %q, %v", out, err)
	}

	addUser(t, st, "{u1}", "Jane Dev", "jane@example.com")
	addUser(t, st, "{u2}", "Joe Ops", "joe@example.com")
	out, err = runUserCmd(t, st, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "EMAIL", "LAST LOGIN", "Jane Dev", "jane@example.com", "Joe Ops"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestUserRemove(t *testing.T) {
	ctx := context.Background()
	st := storemem.New()
	u := addUser(t, st, "{u1}", "Jane Dev", "jane@example.com")

	sum := sha256.Sum256([]byte("tok"))
	hash := hex.EncodeToString(sum[:])
	if err := st.CreateSession(ctx, &store.Session{
		TokenHash: hash, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := runUserCmd(t, st, "remove"); err == nil {
		t.Error("remove without -email must fail")
	}
	if _, err := runUserCmd(t, st, "remove", "-email", "nobody@example.com"); err == nil {
		t.Error("remove of unknown email must fail")
	}

	out, err := runUserCmd(t, st, "remove", "-email", "jane@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed 1 user(s)") {
		t.Errorf("output: %s", out)
	}
	// R5: the user is gone and their session is dead immediately.
	if _, err := st.UserByID(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("user still present: %v", err)
	}
	if _, err := st.UserBySession(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("session still authenticates: %v", err)
	}
}
