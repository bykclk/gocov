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
		INSERT INTO repos (forge, slug, token, default_branch, forge_credentials,
			min_coverage, min_diff_coverage, max_coverage_drop)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`,
		r.Forge, r.Slug, r.Token, r.DefaultBranch, creds,
		r.Gate.MinCoverage, r.Gate.MinDiffCoverage, r.Gate.MaxCoverageDrop,
	).Scan(&r.ID, &r.CreatedAt)
}

func (s *Store) UpdateRepo(ctx context.Context, r *store.Repo) error {
	creds, err := marshalCreds(r.ForgeCredentials)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE repos SET forge = $2, slug = $3, token = $4,
			default_branch = $5, forge_credentials = $6,
			min_coverage = $7, min_diff_coverage = $8, max_coverage_drop = $9
		WHERE id = $1`,
		r.ID, r.Forge, r.Slug, r.Token, r.DefaultBranch, creds,
		r.Gate.MinCoverage, r.Gate.MinDiffCoverage, r.Gate.MaxCoverageDrop)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const repoCols = `id, forge, slug, token, default_branch, COALESCE(forge_credentials, 'null'::jsonb),
	min_coverage, min_diff_coverage, max_coverage_drop, created_at`

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
	err := row.Scan(&r.ID, &r.Forge, &r.Slug, &r.Token, &r.DefaultBranch, &creds,
		&r.Gate.MinCoverage, &r.Gate.MinDiffCoverage, &r.Gate.MaxCoverageDrop, &r.CreatedAt)
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

const workspaceCols = `id, forge, prefix, token, default_branch,
	min_coverage, min_diff_coverage, max_coverage_drop, created_at`

func (s *Store) CreateWorkspace(ctx context.Context, w *store.Workspace) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO workspaces (forge, prefix, token, default_branch,
			min_coverage, min_diff_coverage, max_coverage_drop)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		w.Forge, w.Prefix, w.Token, w.DefaultBranch,
		w.Gate.MinCoverage, w.Gate.MinDiffCoverage, w.Gate.MaxCoverageDrop,
	).Scan(&w.ID, &w.CreatedAt)
}

func (s *Store) UpdateWorkspace(ctx context.Context, w *store.Workspace) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workspaces SET forge = $2, prefix = $3, token = $4, default_branch = $5,
			min_coverage = $6, min_diff_coverage = $7, max_coverage_drop = $8
		WHERE id = $1`,
		w.ID, w.Forge, w.Prefix, w.Token, w.DefaultBranch,
		w.Gate.MinCoverage, w.Gate.MinDiffCoverage, w.Gate.MaxCoverageDrop)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) WorkspaceByPrefix(ctx context.Context, prefix string) (*store.Workspace, error) {
	return s.scanWorkspace(s.pool.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE prefix = $1`, prefix))
}

func (s *Store) WorkspaceByToken(ctx context.Context, token string) (*store.Workspace, error) {
	return s.scanWorkspace(s.pool.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE token = $1`, token))
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]*store.Workspace, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workspaceCols+` FROM workspaces ORDER BY prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Workspace
	for rows.Next() {
		w, err := s.scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) scanWorkspace(row rowScanner) (*store.Workspace, error) {
	var w store.Workspace
	err := row.Scan(&w.ID, &w.Forge, &w.Prefix, &w.Token, &w.DefaultBranch,
		&w.Gate.MinCoverage, &w.Gate.MinDiffCoverage, &w.Gate.MaxCoverageDrop, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) SetUserWorkspaces(ctx context.Context, userID int64, workspaceIDs []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Drop memberships the forge no longer reports. An empty set matches
	// every row (<> ALL of nothing is true), clearing the user entirely.
	if _, err := tx.Exec(ctx,
		`DELETE FROM workspace_members WHERE user_id = $1 AND workspace_id <> ALL($2)`,
		userID, workspaceIDs); err != nil {
		return err
	}
	// Add the current set; rows that already exist stay untouched.
	for _, wsID := range workspaceIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`,
			wsID, userID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListWorkspacesForUser(ctx context.Context, userID int64) ([]*store.Workspace, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.forge, w.prefix, w.token, w.default_branch,
			w.min_coverage, w.min_diff_coverage, w.max_coverage_drop, w.created_at
		FROM workspaces w
		JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = $1
		ORDER BY w.prefix`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Workspace
	for rows.Next() {
		w, err := s.scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

const userCols = `id, forge, forge_uuid, email, display_name, created_at, last_login_at`

func (s *Store) UpsertUser(ctx context.Context, u *store.User) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO users (forge, forge_uuid, email, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (forge, forge_uuid) DO UPDATE
			SET email = EXCLUDED.email,
				display_name = EXCLUDED.display_name,
				last_login_at = now()
		RETURNING id, created_at, last_login_at`,
		u.Forge, u.ForgeUUID, u.Email, u.DisplayName,
	).Scan(&u.ID, &u.CreatedAt, &u.LastLoginAt)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*store.User, error) {
	return s.scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]*store.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.User
	for rows.Next() {
		u, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	// Sessions go with the user via ON DELETE CASCADE.
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) scanUser(row rowScanner) (*store.User, error) {
	var u store.User
	err := row.Scan(&u.ID, &u.Forge, &u.ForgeUUID, &u.Email, &u.DisplayName,
		&u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) CreateSession(ctx context.Context, sess *store.Session) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, $3)
		RETURNING created_at`,
		sess.TokenHash, sess.UserID, sess.ExpiresAt,
	).Scan(&sess.CreatedAt)
}

func (s *Store) UserBySession(ctx context.Context, tokenHash string) (*store.User, error) {
	// Expired sessions are simply never matched; rows are cleaned up lazily
	// when the same token is presented again.
	u, err := s.scanUser(s.pool.QueryRow(ctx, `
		SELECT u.id, u.forge, u.forge_uuid, u.email, u.display_name, u.created_at, u.last_login_at
		FROM users u JOIN sessions s ON s.user_id = u.id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		tokenHash))
	if errors.Is(err, store.ErrNotFound) {
		_, _ = s.pool.Exec(ctx,
			`DELETE FROM sessions WHERE token_hash = $1 AND expires_at <= now()`, tokenHash)
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
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
			total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage, gate_failed, path_prefix)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, created_at`,
		u.RepoID, u.CommitSHA, u.Branch, u.PRID, u.Format,
		u.TotalPct, u.CoveredStmts, u.TotalStmts, u.RawBlobKey, diffCov, u.GateFailed, u.PathPrefix,
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
	total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage, gate_failed, path_prefix, created_at`

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

func (s *Store) ListBranchUploads(ctx context.Context, repoID int64, branch string, limit int) ([]*store.Upload, error) {
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+uploadCols+` FROM uploads
		 WHERE repo_id = $1 AND branch = $2 ORDER BY id DESC LIMIT $3`,
		repoID, branch, lim)
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

func (s *Store) LatestPassedUpload(ctx context.Context, repoID int64, branch string) (*store.Upload, error) {
	return s.scanUpload(s.pool.QueryRow(ctx,
		`SELECT `+uploadCols+` FROM uploads
		 WHERE repo_id = $1 AND branch = $2 AND NOT gate_failed
		 ORDER BY id DESC LIMIT 1`,
		repoID, branch))
}

func (s *Store) scanUpload(row rowScanner) (*store.Upload, error) {
	var u store.Upload
	var diffCov []byte
	err := row.Scan(&u.ID, &u.RepoID, &u.CommitSHA, &u.Branch, &u.PRID, &u.Format,
		&u.TotalPct, &u.CoveredStmts, &u.TotalStmts, &u.RawBlobKey, &diffCov, &u.GateFailed, &u.PathPrefix, &u.CreatedAt)
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
