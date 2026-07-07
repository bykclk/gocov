package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/bykclk/gocov/internal/blobstore"
	blobpg "github.com/bykclk/gocov/internal/blobstore/postgres"
	storepg "github.com/bykclk/gocov/internal/store/postgres"
	"github.com/bykclk/gocov/internal/testpg"
)

func TestBlobstore(t *testing.T) {
	pool := testpg.Pool(t)
	ctx := context.Background()
	// The blobs table is owned by the store migrations.
	if err := storepg.New(pool).Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	bs := blobpg.New(pool)

	if err := bs.Put(ctx, "k1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := bs.Get(ctx, "k1")
	if err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("Get = %q, %v", got, err)
	}

	// Put overwrites.
	if err := bs.Put(ctx, "k1", []byte("world")); err != nil {
		t.Fatal(err)
	}
	if got, _ = bs.Get(ctx, "k1"); !bytes.Equal(got, []byte("world")) {
		t.Errorf("after overwrite: %q", got)
	}

	// Missing keys.
	if _, err := bs.Get(ctx, "nope"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Errorf("Get missing = %v", err)
	}

	// Delete is idempotent.
	if err := bs.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Get(ctx, "k1"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Error("blob survived delete")
	}
	if err := bs.Delete(ctx, "k1"); err != nil {
		t.Errorf("second delete = %v", err)
	}
}
