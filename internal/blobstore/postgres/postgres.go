// Package postgres implements blobstore.Store on a Postgres bytea column.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bykclk/gocov/internal/blobstore"
)

// Store implements blobstore.Store on the blobs table (see store/postgres
// migrations, which own the schema).
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing pool, typically shared with the main store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO blobs (key, data) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data`, key, data)
	return err
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM blobs WHERE key = $1`, key).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, blobstore.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM blobs WHERE key = $1`, key)
	return err
}

var _ blobstore.Store = (*Store)(nil)
