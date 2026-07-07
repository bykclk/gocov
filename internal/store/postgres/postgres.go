// Package postgres implements store.Store on PostgreSQL via pgx.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bykclk/gocov/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store implements store.Store on a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing pool. The caller owns the pool's lifecycle.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool so other Postgres-backed components
// (e.g. the blobstore) can share it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies all embedded migrations that have not been applied yet,
// in filename order. Safe to run on every startup.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateRepo(ctx context.Context, r *store.Repo) error {
	creds, err := marshalCreds(r.ForgeCredentials)
	if err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `
		INSERT INTO repos (forge, slug, token, default_branch, forge_credentials)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		r.Forge, r.Slug, r.Token, r.DefaultBranch, creds,
	).Scan(&r.ID, &r.CreatedAt)
}

func (s *Store) UpdateRepo(ctx context.Context, r *store.Repo) error {
	creds, err := marshalCreds(r.ForgeCredentials)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE repos SET forge = $2, slug = $3, token = $4,
			default_branch = $5, forge_credentials = $6
		WHERE id = $1`,
		r.ID, r.Forge, r.Slug, r.Token, r.DefaultBranch, creds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const repoCols = `id, forge, slug, token, default_branch, COALESCE(forge_credentials, 'null'::jsonb), created_at`

func (s *Store) DeleteRepo(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RepoByID(ctx context.Context, id int64) (*store.Repo, error) {
	return s.scanRepo(s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE id = $1`, id))
}

func (s *Store) RepoBySlug(ctx context.Context, slug string) (*store.Repo, error) {
	return s.scanRepo(s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE slug = $1`, slug))
}

func (s *Store) RepoByToken(ctx context.Context, token string) (*store.Repo, error) {
	return s.scanRepo(s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE token = $1`, token))
}

func (s *Store) ListRepos(ctx context.Context) ([]*store.Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+repoCols+` FROM repos ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Repo
	for rows.Next() {
		r, err := s.scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanRepo(row rowScanner) (*store.Repo, error) {
	var r store.Repo
	var creds []byte
	err := row.Scan(&r.ID, &r.Forge, &r.Slug, &r.Token, &r.DefaultBranch, &creds, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(creds) > 0 && string(creds) != "null" {
		if err := json.Unmarshal(creds, &r.ForgeCredentials); err != nil {
			return nil, fmt.Errorf("repo %s: bad forge_credentials: %w", r.Slug, err)
		}
	}
	return &r, nil
}

func marshalCreds(creds map[string]string) ([]byte, error) {
	if len(creds) == 0 {
		return nil, nil
	}
	return json.Marshal(creds)
}

func (s *Store) CreateUpload(ctx context.Context, u *store.Upload, files []*store.UploadFile) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var diffCov []byte
	if u.DiffCoverage != nil {
		if diffCov, err = json.Marshal(u.DiffCoverage); err != nil {
			return err
		}
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO uploads (repo_id, commit_sha, branch, pr_id, format,
			total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at`,
		u.RepoID, u.CommitSHA, u.Branch, u.PRID, u.Format,
		u.TotalPct, u.CoveredStmts, u.TotalStmts, u.RawBlobKey, diffCov,
	).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		return err
	}

	for _, f := range files {
		f.UploadID = u.ID
		blocks, err := json.Marshal(f.Blocks)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO upload_files (upload_id, path, pct, covered_stmts, total_stmts, blocks)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			f.UploadID, f.Path, f.Pct, f.CoveredStmts, f.TotalStmts, blocks)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const uploadCols = `id, repo_id, commit_sha, branch, pr_id, format,
	total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage, created_at`

func (s *Store) Upload(ctx context.Context, id int64) (*store.Upload, error) {
	return s.scanUpload(s.pool.QueryRow(ctx,
		`SELECT `+uploadCols+` FROM uploads WHERE id = $1`, id))
}

func (s *Store) ListUploads(ctx context.Context, repoID int64, limit int) ([]*store.Upload, error) {
	// LIMIT NULL means no limit in Postgres.
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+uploadCols+` FROM uploads WHERE repo_id = $1 ORDER BY id DESC LIMIT $2`,
		repoID, lim)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Upload
	for rows.Next() {
		u, err := s.scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) LatestUpload(ctx context.Context, repoID int64, branch string) (*store.Upload, error) {
	return s.scanUpload(s.pool.QueryRow(ctx,
		`SELECT `+uploadCols+` FROM uploads
		 WHERE repo_id = $1 AND branch = $2 ORDER BY id DESC LIMIT 1`,
		repoID, branch))
}

func (s *Store) scanUpload(row rowScanner) (*store.Upload, error) {
	var u store.Upload
	var diffCov []byte
	err := row.Scan(&u.ID, &u.RepoID, &u.CommitSHA, &u.Branch, &u.PRID, &u.Format,
		&u.TotalPct, &u.CoveredStmts, &u.TotalStmts, &u.RawBlobKey, &diffCov, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(diffCov) > 0 {
		if err := json.Unmarshal(diffCov, &u.DiffCoverage); err != nil {
			return nil, fmt.Errorf("upload %d: bad diff_coverage: %w", u.ID, err)
		}
	}
	return &u, nil
}

func (s *Store) UploadFiles(ctx context.Context, uploadID int64) ([]*store.UploadFile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT upload_id, path, pct, covered_stmts, total_stmts, blocks
		FROM upload_files WHERE upload_id = $1 ORDER BY path`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.UploadFile
	for rows.Next() {
		var f store.UploadFile
		var blocks []byte
		if err := rows.Scan(&f.UploadID, &f.Path, &f.Pct, &f.CoveredStmts, &f.TotalStmts, &blocks); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(blocks, &f.Blocks); err != nil {
			return nil, fmt.Errorf("upload %d file %s: bad blocks: %w", uploadID, f.Path, err)
		}
		out = append(out, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		// Distinguish "upload has no files" from "upload does not exist"
		// is not needed by callers; return empty slice.
		out = []*store.UploadFile{}
	}
	return out, nil
}

// ensure interface compliance
var _ store.Store = (*Store)(nil)
