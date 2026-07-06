// Package blobstore abstracts raw-profile persistence so it can later move
// to object storage (S3/R2) without touching callers.
package blobstore

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get for missing keys.
var ErrNotFound = errors.New("blobstore: not found")

// Store is a minimal blob store.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
