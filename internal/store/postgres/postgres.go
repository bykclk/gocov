// Package postgres implements store.Store on PostgreSQL via pgx.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocov/gocov/internal/secretbox"
	"github.com/gocov/gocov/internal/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store implements store.Store on a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
	// cipher seals the Bitbucket grant refresh token at rest (One-Click
	// Connect D6). Nil when GOCOV_SECRET_KEY is not configured — storing
	// a grant then fails loudly, but everything else works.
	cipher *secretbox.Box
}

// New wraps an existing pool. The caller owns the pool's lifecycle.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetCipher installs the at-rest cipher for grant refresh tokens.
func (s *Store) SetCipher(box *secretbox.Box) { s.cipher = box }

// sealToken encrypts a refresh token for storage; empty stays empty.
func (s *Store) sealToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if s.cipher == nil {
		return "", fmt.Errorf("postgres: storing a grant token requires GOCOV_SECRET_KEY")
	}
	return s.cipher.Seal(token)
}

// openToken decrypts a stored refresh token. Failures (missing or rotated
// GOCOV_SECRET_KEY, tampering) must not brick every workspace read that
// happens to include the row — the token comes back empty with ok=false,
// and the caller marks the loaded struct broken so the UI asks for a
// reconnect, which re-seals under the current key.
func (s *Store) openToken(sealed string) (token string, ok bool) {
	if sealed == "" {
		return "", true
	}
	if s.cipher == nil {
		return "", false
	}
	plain, err := s.cipher.Open(sealed)
	if err != nil {
		return "", false
	}
	return plain, true
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
	slices.Sort(names)

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

// CreateRepo leaves visibility_checked_at at its NULL default: per the
// Store contract only SetRepoVisibility writes the stamp, so a fresh row
// always starts as "never asked".
func (s *Store) CreateRepo(ctx context.Context, r *store.Repo) error {
	return s.pool.QueryRow(ctx, `
		INSERT INTO repos (forge, slug, token, default_branch,
			min_coverage, min_diff_coverage, max_coverage_drop,
			visibility, public_reports_disabled, ignore_paths)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at`,
		r.Forge, r.Slug, r.Token, r.DefaultBranch,
		r.Gate.MinCoverage, r.Gate.MinDiffCoverage, r.Gate.MaxCoverageDrop,
		r.Visibility, r.PublicReportsDisabled, textArray(r.IgnorePaths),
	).Scan(&r.ID, &r.CreatedAt)
}

// UpdateRepo leaves the visibility columns (value and checked-at stamp)
// alone: they are written from the upload path via SetRepoVisibility, and
// a settings save carrying the values it loaded minutes ago must not be
// able to revert a concurrent refresh (a private repo would reopen to
// anonymous visitors).
func (s *Store) UpdateRepo(ctx context.Context, r *store.Repo) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE repos SET forge = $2, slug = $3, token = $4,
			default_branch = $5,
			min_coverage = $6, min_diff_coverage = $7, max_coverage_drop = $8,
			public_reports_disabled = $9, ignore_paths = $10
		WHERE id = $1`,
		r.ID, r.Forge, r.Slug, r.Token, r.DefaultBranch,
		r.Gate.MinCoverage, r.Gate.MinDiffCoverage, r.Gate.MaxCoverageDrop,
		r.PublicReportsDisabled, textArray(r.IgnorePaths))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) PublicRepoRefs(ctx context.Context, limit int) ([]store.RepoRef, error) {
	q := `SELECT forge, slug FROM repos
		WHERE visibility = 'public' AND NOT public_reports_disabled
		ORDER BY forge, slug`
	var args []any
	if limit > 0 {
		q += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.RepoRef
	for rows.Next() {
		var ref store.RepoRef
		if err := rows.Scan(&ref.Forge, &ref.Slug); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SetRepoVisibility stamps checkedAt as given (the app clock, the same
// clock the freshness windows compare against) rather than now(): the
// caller took it before asking the forge, so of two racing answers the
// later ask wins regardless of write order.
func (s *Store) SetRepoVisibility(ctx context.Context, repoID int64, visibility string, checkedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE repos SET visibility = $2, visibility_checked_at = $3
		WHERE id = $1 AND (visibility_checked_at IS NULL OR visibility_checked_at < $3)`,
		repoID, visibility, checkedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Missing repo, or a fresher answer already landed — only the
		// former is an error.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM repos WHERE id = $1)`, repoID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return store.ErrNotFound
		}
	}
	return nil
}

const repoCols = `id, forge, slug, token, default_branch,
	min_coverage, min_diff_coverage, max_coverage_drop,
	visibility, public_reports_disabled, visibility_checked_at, ignore_paths, created_at`

// textArray is a nil-safe text[] parameter: a nil slice would bind as
// NULL and trip the column's NOT NULL constraint.
func textArray(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

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

func (s *Store) RepoBySlug(ctx context.Context, forge, slug string) (*store.Repo, error) {
	return s.scanRepo(s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE forge = $1 AND slug = $2`, forge, slug))
}

func (s *Store) RepoByToken(ctx context.Context, token string) (*store.Repo, error) {
	return s.scanRepo(s.pool.QueryRow(ctx,
		`SELECT `+repoCols+` FROM repos WHERE token = $1`, token))
}

func (s *Store) ListRepos(ctx context.Context) ([]*store.Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+repoCols+` FROM repos ORDER BY forge, slug`)
	if err != nil {
		return nil, err
	}
	return collect(rows, s.scanRepo)
}

type rowScanner interface {
	Scan(dest ...any) error
}

// collect drains rows through scan — one of the scanX helpers, which
// read a row into a struct — and closes them either way. It is the body
// of every List* query.
func collect[T any](rows pgx.Rows, scan func(rowScanner) (*T, error)) ([]*T, error) {
	defer rows.Close()
	var out []*T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// querier is the subset of pgx shared by *pgxpool.Pool and pgx.Tx, so the
// commit-report queries can run either directly on the pool or inside the
// locked transaction that WithCommitReportTx opens.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Store) scanRepo(row rowScanner) (*store.Repo, error) {
	var r store.Repo
	var checkedAt *time.Time // NULL = the forge has never answered
	err := row.Scan(&r.ID, &r.Forge, &r.Slug, &r.Token, &r.DefaultBranch,
		&r.Gate.MinCoverage, &r.Gate.MinDiffCoverage, &r.Gate.MaxCoverageDrop,
		&r.Visibility, &r.PublicReportsDisabled, &checkedAt, &r.IgnorePaths, &r.CreatedAt)
	if checkedAt != nil {
		r.VisibilityCheckedAt = *checkedAt
	}
	if len(r.IgnorePaths) == 0 {
		r.IgnorePaths = nil // an empty array reads back as the zero value
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

const workspaceCols = `id, forge, prefix, token, default_branch,
	min_coverage, min_diff_coverage, max_coverage_drop, report_retention_days,
	github_installation_id, github_app_broken,
	bitbucket_grant_account, bitbucket_refresh_token, bitbucket_grant_broken,
	gitlab_grant_account, gitlab_refresh_token, gitlab_grant_broken, created_at`

func (s *Store) CreateWorkspace(ctx context.Context, w *store.Workspace) error {
	return s.createWorkspace(ctx, s.pool, w)
}

// execer is the subset of pgx querying shared by pools and transactions.
type execer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) createWorkspace(ctx context.Context, db execer, w *store.Workspace) error {
	sealed, err := s.sealToken(w.BitbucketRefreshToken)
	if err != nil {
		return err
	}
	sealedGL, err := s.sealToken(w.GitLabRefreshToken)
	if err != nil {
		return err
	}
	return db.QueryRow(ctx, `
		INSERT INTO workspaces (forge, prefix, token, default_branch,
			min_coverage, min_diff_coverage, max_coverage_drop, report_retention_days,
			github_installation_id, github_app_broken,
			bitbucket_grant_account, bitbucket_refresh_token, bitbucket_grant_broken,
			gitlab_grant_account, gitlab_refresh_token, gitlab_grant_broken)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id, created_at`,
		w.Forge, w.Prefix, w.Token, w.DefaultBranch,
		w.Gate.MinCoverage, w.Gate.MinDiffCoverage, w.Gate.MaxCoverageDrop, w.ReportRetentionDays,
		w.GitHubInstallationID, w.GitHubAppBroken,
		w.BitbucketGrantAccount, sealed, w.BitbucketGrantBroken,
		w.GitLabGrantAccount, sealedGL, w.GitLabGrantBroken,
	).Scan(&w.ID, &w.CreatedAt)
}

func (s *Store) RegisterWorkspace(ctx context.Context, w *store.Workspace, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := s.createWorkspace(ctx, tx, w); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		w.ID, userID, store.RoleOwner); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateWorkspace replaces the stored row with w's fields — except the
// Bitbucket and GitLab grant columns, which only their SetWorkspace*Grant
// methods touch: the refresh tokens rotate on every use, so a full-row
// write from an earlier read would resurrect an already-invalidated
// token.
func (s *Store) UpdateWorkspace(ctx context.Context, w *store.Workspace) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workspaces SET forge = $2, prefix = $3, token = $4, default_branch = $5,
			min_coverage = $6, min_diff_coverage = $7, max_coverage_drop = $8,
			report_retention_days = $9, github_installation_id = $10, github_app_broken = $11
		WHERE id = $1`,
		w.ID, w.Forge, w.Prefix, w.Token, w.DefaultBranch,
		w.Gate.MinCoverage, w.Gate.MinDiffCoverage, w.Gate.MaxCoverageDrop,
		w.ReportRetentionDays, w.GitHubInstallationID, w.GitHubAppBroken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SetWorkspaceBitbucketGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return s.setWorkspaceGrant(ctx, s.pool, setBitbucketGrantSQL, workspaceID, account, refreshToken, broken)
}

func (s *Store) SetWorkspaceGitLabGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return s.setWorkspaceGrant(ctx, s.pool, setGitLabGrantSQL, workspaceID, account, refreshToken, broken)
}

// The two grant columns sets differ only by forge; setWorkspaceGrant is
// the write behind both, with the statement chosen by the caller.
const (
	setBitbucketGrantSQL = `
		UPDATE workspaces SET bitbucket_grant_account = $2,
			bitbucket_refresh_token = $3, bitbucket_grant_broken = $4
		WHERE id = $1`
	setGitLabGrantSQL = `
		UPDATE workspaces SET gitlab_grant_account = $2,
			gitlab_refresh_token = $3, gitlab_grant_broken = $4
		WHERE id = $1`
)

func (s *Store) setWorkspaceGrant(ctx context.Context, q querier, sql string, workspaceID int64, account, refreshToken string, broken bool) error {
	sealed, err := s.sealToken(refreshToken)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, sql, workspaceID, account, sealed, broken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The workspace prefix decides which repos belong to it — repos carry
	// no workspace_id, the tie is the slug prefix on the same forge (M2
	// convention). Delete those repos first; uploads, upload_files and
	// commit_reports cascade off repos(id). Membership rows cascade off
	// the workspace row below.
	var forge, prefix string
	if err := tx.QueryRow(ctx,
		`SELECT forge, prefix FROM workspaces WHERE id = $1`, id).Scan(&forge, &prefix); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM repos WHERE forge = $1 AND slug LIKE $2 ESCAPE '\'`, forge, likePrefix(prefix)+`/%`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// likePrefix escapes the LIKE metacharacters in a literal prefix so it can
// be used as the fixed head of a `<prefix>/%` pattern (ESCAPE '\').
func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Store) WorkspaceByPrefix(ctx context.Context, forge, prefix string) (*store.Workspace, error) {
	return s.workspaceByPrefix(ctx, s.pool, forge, prefix)
}

func (s *Store) workspaceByPrefix(ctx context.Context, q querier, forge, prefix string) (*store.Workspace, error) {
	return s.scanWorkspace(q.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE forge = $1 AND prefix = $2`, forge, prefix))
}

// WithGrantLock runs fn inside a transaction holding a transaction-scoped
// advisory lock keyed by the workspace, so a grant refresh serializes
// across all server instances sharing the database — the cross-process
// guarantee a rolling deploy needs, since Bitbucket and GitLab rotate the
// refresh token on every use. fn's read of the stored token and its write
// of the rotated one go through the locked transaction's own connection,
// so a burst of cold-cache uploads of one workspace cannot deadlock the
// pool the way a lock-on-one-connection, query-on-another design would
// (see WithCommitReportTx). The lock is released when the transaction
// ends, on commit or rollback.
func (s *Store) WithGrantLock(ctx context.Context, workspaceID int64, fn func(context.Context, store.GrantTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey("grant", workspaceID, "")); err != nil {
		return err
	}
	if err := fn(ctx, &grantTx{s: s, tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// grantTx binds a refresh's token read and rotated-token write to one
// locked transaction. It satisfies store.GrantTx.
type grantTx struct {
	s  *Store
	tx pgx.Tx
}

func (g *grantTx) WorkspaceByPrefix(ctx context.Context, forge, prefix string) (*store.Workspace, error) {
	return g.s.workspaceByPrefix(ctx, g.tx, forge, prefix)
}

func (g *grantTx) SetWorkspaceBitbucketGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return g.s.setWorkspaceGrant(ctx, g.tx, setBitbucketGrantSQL, workspaceID, account, refreshToken, broken)
}

func (g *grantTx) SetWorkspaceGitLabGrant(ctx context.Context, workspaceID int64, account, refreshToken string, broken bool) error {
	return g.s.setWorkspaceGrant(ctx, g.tx, setGitLabGrantSQL, workspaceID, account, refreshToken, broken)
}

func (s *Store) WorkspaceByToken(ctx context.Context, token string) (*store.Workspace, error) {
	return s.scanWorkspace(s.pool.QueryRow(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE token = $1`, token))
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]*store.Workspace, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+workspaceCols+` FROM workspaces ORDER BY forge, prefix`)
	if err != nil {
		return nil, err
	}
	return collect(rows, s.scanWorkspace)
}

func (s *Store) scanWorkspace(row rowScanner) (*store.Workspace, error) {
	var w store.Workspace
	var sealedRefresh, sealedGLRefresh string
	err := row.Scan(&w.ID, &w.Forge, &w.Prefix, &w.Token, &w.DefaultBranch,
		&w.Gate.MinCoverage, &w.Gate.MinDiffCoverage, &w.Gate.MaxCoverageDrop, &w.ReportRetentionDays,
		&w.GitHubInstallationID, &w.GitHubAppBroken,
		&w.BitbucketGrantAccount, &sealedRefresh, &w.BitbucketGrantBroken,
		&w.GitLabGrantAccount, &sealedGLRefresh, &w.GitLabGrantBroken, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if token, ok := s.openToken(sealedRefresh); ok {
		w.BitbucketRefreshToken = token
	} else {
		// Undecryptable (key rotated away, or missing): surface as a
		// broken connection instead of failing every workspace read.
		w.BitbucketGrantBroken = true
	}
	if token, ok := s.openToken(sealedGLRefresh); ok {
		w.GitLabRefreshToken = token
	} else {
		w.GitLabGrantBroken = true
	}
	return &w, nil
}

func (s *Store) SetUserMemberships(ctx context.Context, userID int64, memberships []store.Membership) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Drop memberships the forge no longer reports. An empty set matches
	// every row (<> ALL of nothing is true), clearing the user entirely.
	ids := make([]int64, len(memberships))
	for i, m := range memberships {
		ids[i] = m.WorkspaceID
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM workspace_members WHERE user_id = $1 AND workspace_id <> ALL($2)`,
		userID, ids); err != nil {
		return err
	}
	// Add the current set; a row that already exists only has its role
	// brought up to date, so a same-set re-sync leaves nothing changed.
	for _, m := range memberships {
		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
				ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
			m.WorkspaceID, userID, m.Role); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListMembershipsForUser(ctx context.Context, userID int64) ([]store.Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, role FROM workspace_members
		WHERE user_id = $1 ORDER BY workspace_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Membership
	for rows.Next() {
		var m store.Membership
		if err := rows.Scan(&m.WorkspaceID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListWorkspacesForUser(ctx context.Context, userID int64) ([]*store.Workspace, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.forge, w.prefix, w.token, w.default_branch,
			w.min_coverage, w.min_diff_coverage, w.max_coverage_drop, w.report_retention_days,
			w.github_installation_id, w.github_app_broken,
			w.bitbucket_grant_account, w.bitbucket_refresh_token, w.bitbucket_grant_broken,
			w.gitlab_grant_account, w.gitlab_refresh_token, w.gitlab_grant_broken, w.created_at
		FROM workspaces w
		JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = $1
		ORDER BY w.forge, w.prefix`, userID)
	if err != nil {
		return nil, err
	}
	return collect(rows, s.scanWorkspace)
}

const userCols = `id, forge, forge_uuid, email, display_name,
	COALESCE(forge_workspaces, 'null'::jsonb), COALESCE(forge_owned_workspaces, 'null'::jsonb),
	created_at, last_login_at`

func (s *Store) UpsertUser(ctx context.Context, u *store.User) error {
	// The forge workspace snapshot is replaced wholesale: the identity
	// fetch either succeeded as a whole or failed the login, so an empty
	// list here is the forge's answer, not a partial read.
	wss, err := marshalStrings(u.ForgeWorkspaces)
	if err != nil {
		return err
	}
	owned, err := marshalStrings(u.ForgeOwnedWorkspaces)
	if err != nil {
		return err
	}
	return s.pool.QueryRow(ctx, `
		INSERT INTO users (forge, forge_uuid, email, display_name, forge_workspaces, forge_owned_workspaces)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (forge, forge_uuid) DO UPDATE
			SET email = EXCLUDED.email,
				display_name = EXCLUDED.display_name,
				forge_workspaces = EXCLUDED.forge_workspaces,
				forge_owned_workspaces = EXCLUDED.forge_owned_workspaces,
				last_login_at = now()
		RETURNING id, created_at, last_login_at`,
		u.Forge, u.ForgeUUID, u.Email, u.DisplayName, wss, owned,
	).Scan(&u.ID, &u.CreatedAt, &u.LastLoginAt)
}

func marshalStrings(ss []string) ([]byte, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	return json.Marshal(ss)
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
	return collect(rows, s.scanUser)
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
	var wss, owned []byte
	err := row.Scan(&u.ID, &u.Forge, &u.ForgeUUID, &u.Email, &u.DisplayName,
		&wss, &owned, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := unmarshalStrings(wss, &u.ForgeWorkspaces); err != nil {
		return nil, fmt.Errorf("user %d: bad forge_workspaces: %w", u.ID, err)
	}
	if err := unmarshalStrings(owned, &u.ForgeOwnedWorkspaces); err != nil {
		return nil, fmt.Errorf("user %d: bad forge_owned_workspaces: %w", u.ID, err)
	}
	return &u, nil
}

// unmarshalStrings is marshalStrings' inverse: a NULL column (read as
// "null") leaves dst nil.
func unmarshalStrings(raw []byte, dst *[]string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
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
		SELECT u.id, u.forge, u.forge_uuid, u.email, u.display_name,
			COALESCE(u.forge_workspaces, 'null'::jsonb), COALESCE(u.forge_owned_workspaces, 'null'::jsonb),
			u.created_at, u.last_login_at
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
	meta, err := marshalUploadMeta(u.Meta)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO uploads (repo_id, commit_sha, branch, pr_id, format,
			total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage, gate_failed, path_prefix, part, meta)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id, created_at`,
		u.RepoID, u.CommitSHA, u.Branch, u.PRID, u.Format,
		u.TotalPct, u.CoveredStmts, u.TotalStmts, u.RawBlobKey, diffCov, u.GateFailed, u.PathPrefix, u.Part, meta,
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
	total_pct, covered_stmts, total_stmts, raw_blob_key, diff_coverage, gate_failed, path_prefix, part, created_at, meta`

// marshalUploadMeta encodes upload provenance for storage, returning nil
// (SQL NULL) when nothing was captured so empty uploads stay compact.
func marshalUploadMeta(m store.UploadMeta) ([]byte, error) {
	if (m == store.UploadMeta{}) {
		return nil, nil
	}
	return json.Marshal(m)
}

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
	return collect(rows, s.scanUpload)
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
	return collect(rows, s.scanUpload)
}

func (s *Store) scanUpload(row rowScanner) (*store.Upload, error) {
	var u store.Upload
	var diffCov, meta []byte
	err := row.Scan(&u.ID, &u.RepoID, &u.CommitSHA, &u.Branch, &u.PRID, &u.Format,
		&u.TotalPct, &u.CoveredStmts, &u.TotalStmts, &u.RawBlobKey, &diffCov, &u.GateFailed, &u.PathPrefix, &u.Part, &u.CreatedAt, &meta)
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
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &u.Meta); err != nil {
			return nil, fmt.Errorf("upload %d: bad meta: %w", u.ID, err)
		}
	}
	return &u, nil
}

func (s *Store) UploadFiles(ctx context.Context, uploadID int64) ([]*store.UploadFile, error) {
	return uploadFiles(ctx, s.pool, uploadID)
}

func uploadFiles(ctx context.Context, q querier, uploadID int64) ([]*store.UploadFile, error) {
	rows, err := q.Query(ctx, `
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

// LatestUploadsPerPart returns the newest upload of each distinct part for a
// commit — the inputs to the merged report. DISTINCT ON keeps only the
// highest id per part, so a re-uploaded part supersedes its predecessors.
func (s *Store) LatestUploadsPerPart(ctx context.Context, repoID int64, commitSHA string) ([]*store.Upload, error) {
	return s.latestUploadsPerPart(ctx, s.pool, repoID, commitSHA)
}

// CommitParts returns the distinct part names for a commit, reading only the
// part column — the parts cap needs a count and a membership test, not the
// full upload rows (which carry blocks and diff-coverage JSON).
func (s *Store) CommitParts(ctx context.Context, repoID int64, commitSHA string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT part FROM uploads WHERE repo_id = $1 AND commit_sha = $2`,
		repoID, commitSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) latestUploadsPerPart(ctx context.Context, q querier, repoID int64, commitSHA string) ([]*store.Upload, error) {
	rows, err := q.Query(ctx,
		`SELECT DISTINCT ON (part) `+uploadCols+` FROM uploads
		 WHERE repo_id = $1 AND commit_sha = $2
		 ORDER BY part, id DESC`,
		repoID, commitSHA)
	if err != nil {
		return nil, err
	}
	return collect(rows, s.scanUpload)
}

// WithCommitReportTx runs fn inside a transaction that holds a
// transaction-scoped Postgres advisory lock keyed by the commit, so the
// recompute of one commit's merged report serializes across all server
// instances sharing the database. Every query fn issues through the passed
// CommitTx runs on that same transaction's single connection — critically,
// the lock does not tie up one pooled connection while the reads reach for a
// second, which would deadlock the pool under enough concurrent uploads of
// one commit. pg_advisory_xact_lock releases automatically when the
// transaction ends, on commit or rollback. Different commits hash to
// different keys and never contend (a collision only costs an occasional
// extra wait, never correctness).
func (s *Store) WithCommitReportTx(ctx context.Context, repoID int64, commitSHA string, fn func(context.Context, store.CommitTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey("recompute", repoID, commitSHA)); err != nil {
		return err
	}
	if err := fn(ctx, &commitReportTx{s: s, tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// commitReportTx binds the recompute's reads and upsert to one locked
// transaction. It satisfies store.CommitTx.
type commitReportTx struct {
	s  *Store
	tx pgx.Tx
}

func (c *commitReportTx) LatestUploadsPerPart(ctx context.Context, repoID int64, commitSHA string) ([]*store.Upload, error) {
	return c.s.latestUploadsPerPart(ctx, c.tx, repoID, commitSHA)
}

func (c *commitReportTx) UploadFiles(ctx context.Context, uploadID int64) ([]*store.UploadFile, error) {
	return uploadFiles(ctx, c.tx, uploadID)
}

func (c *commitReportTx) LatestPassedCommitReport(ctx context.Context, repoID int64, branch, excludeCommit string) (*store.CommitReport, error) {
	return c.s.latestPassedCommitReport(ctx, c.tx, repoID, branch, excludeCommit)
}

func (c *commitReportTx) UpsertCommitReport(ctx context.Context, cr *store.CommitReport) error {
	return c.s.upsertCommitReport(ctx, c.tx, cr)
}

// advisoryKey hashes a namespace and (id, name) into the signed 64-bit
// space Postgres advisory locks use. The namespace keeps the recompute
// lock, the status-push lock and the grant-refresh lock (keyed by
// workspace id, with an empty name) in separate spaces so they never block
// each other for the same id.
func advisoryKey(namespace string, id int64, name string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s:%d:%s", namespace, id, name)
	return int64(h.Sum64()) //nolint:gosec // intentional bit reinterpretation into the advisory key space
}

const commitReportCols = `id, repo_id, commit_sha, branch, pr_id, total_pct,
	covered_stmts, total_stmts, gate_failed, diff_coverage, part_count, upload_id, created_at, updated_at`

func (s *Store) UpsertCommitReport(ctx context.Context, cr *store.CommitReport) error {
	return s.upsertCommitReport(ctx, s.pool, cr)
}

func (s *Store) upsertCommitReport(ctx context.Context, q querier, cr *store.CommitReport) error {
	var diffCov []byte
	if cr.DiffCoverage != nil {
		var err error
		if diffCov, err = json.Marshal(cr.DiffCoverage); err != nil {
			return err
		}
	}
	// The first-seen created_at survives the conflict update; only the
	// derived fields and updated_at move.
	return q.QueryRow(ctx, `
		INSERT INTO commit_reports (repo_id, commit_sha, branch, pr_id, total_pct,
			covered_stmts, total_stmts, gate_failed, diff_coverage, part_count, upload_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (repo_id, commit_sha) DO UPDATE SET
			branch = EXCLUDED.branch,
			pr_id = EXCLUDED.pr_id,
			total_pct = EXCLUDED.total_pct,
			covered_stmts = EXCLUDED.covered_stmts,
			total_stmts = EXCLUDED.total_stmts,
			gate_failed = EXCLUDED.gate_failed,
			diff_coverage = EXCLUDED.diff_coverage,
			part_count = EXCLUDED.part_count,
			upload_id = EXCLUDED.upload_id,
			updated_at = now()
		RETURNING id, created_at, updated_at`,
		cr.RepoID, cr.CommitSHA, cr.Branch, cr.PRID, cr.TotalPct,
		cr.CoveredStmts, cr.TotalStmts, cr.GateFailed, diffCov, cr.PartCount, cr.UploadID,
	).Scan(&cr.ID, &cr.CreatedAt, &cr.UpdatedAt)
}

func (s *Store) CommitReport(ctx context.Context, repoID int64, commitSHA string) (*store.CommitReport, error) {
	return s.scanCommitReport(s.pool.QueryRow(ctx,
		`SELECT `+commitReportCols+` FROM commit_reports WHERE repo_id = $1 AND commit_sha = $2`,
		repoID, commitSHA))
}

func (s *Store) LatestCommitReport(ctx context.Context, repoID int64, branch string) (*store.CommitReport, error) {
	return s.scanCommitReport(s.pool.QueryRow(ctx,
		`SELECT `+commitReportCols+` FROM commit_reports
		 WHERE repo_id = $1 AND branch = $2 ORDER BY id DESC LIMIT 1`,
		repoID, branch))
}

func (s *Store) LatestNonPRCommitReport(ctx context.Context, repoID int64, branch string) (*store.CommitReport, error) {
	return s.scanCommitReport(s.pool.QueryRow(ctx,
		`SELECT `+commitReportCols+` FROM commit_reports
		 WHERE repo_id = $1 AND branch = $2 AND pr_id = '' ORDER BY id DESC LIMIT 1`,
		repoID, branch))
}

func (s *Store) LatestPassedCommitReport(ctx context.Context, repoID int64, branch, excludeCommit string) (*store.CommitReport, error) {
	return s.latestPassedCommitReport(ctx, s.pool, repoID, branch, excludeCommit)
}

func (s *Store) latestPassedCommitReport(ctx context.Context, q querier, repoID int64, branch, excludeCommit string) (*store.CommitReport, error) {
	return s.scanCommitReport(q.QueryRow(ctx,
		`SELECT `+commitReportCols+` FROM commit_reports
		 WHERE repo_id = $1 AND branch = $2 AND commit_sha <> $3 AND NOT gate_failed
		   AND pr_id = ''
		 ORDER BY id DESC LIMIT 1`,
		repoID, branch, excludeCommit))
}

// TryPushStatus serializes the forge status/PR-comment push for one commit
// under a per-commit advisory lock and runs push only when version is at
// least the last successfully pushed version, advancing the stored version
// only after push returns nil. Serializing the whole check-push-advance —
// rather than just claiming a version before an unordered push — means the
// forge can't be left on a stale status by a slow older push landing last,
// and a failed push (push returns an error) leaves the version untouched so
// a later part retries. Returns whether push ran.
//
// push runs while the lock's transaction is held, so it must be bounded by
// ctx; it performs the forge HTTP itself. When no report row exists yet
// there is nothing to attach a status to and push does not run.
func (s *Store) TryPushStatus(ctx context.Context, repoID int64, commitSHA string, version int64, push func(context.Context) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryKey("status", repoID, commitSHA)); err != nil {
		return false, err
	}
	var cur int64
	err = tx.QueryRow(ctx,
		"SELECT status_pushed_version FROM commit_reports WHERE repo_id = $1 AND commit_sha = $2",
		repoID, commitSHA).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // no report to attach a status to
	}
	if err != nil {
		return false, err
	}
	if version < cur {
		return false, nil // a newer push already owns the status
	}
	if err := push(ctx); err != nil {
		return false, err // rolled back: version not advanced, so a retry can push
	}
	if _, err := tx.Exec(ctx,
		"UPDATE commit_reports SET status_pushed_version = $3 WHERE repo_id = $1 AND commit_sha = $2",
		repoID, commitSHA, version); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) ClaimTokenlessUpload(ctx context.Context, repoID, runID, runAttempt int64, part string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO tokenless_claims (repo_id, run_id, run_attempt, part)
		 VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		repoID, runID, runAttempt, part)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) ReleaseTokenlessUpload(ctx context.Context, repoID, runID, runAttempt int64, part string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM tokenless_claims
		 WHERE repo_id = $1 AND run_id = $2 AND run_attempt = $3 AND part = $4`,
		repoID, runID, runAttempt, part)
	return err
}

func (s *Store) ListBranchCommitReports(ctx context.Context, repoID int64, branch string, limit int) ([]*store.CommitReport, error) {
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+commitReportCols+` FROM commit_reports
		 WHERE repo_id = $1 AND branch = $2 ORDER BY id DESC LIMIT $3`,
		repoID, branch, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, s.scanCommitReport)
}

func (s *Store) scanCommitReport(row rowScanner) (*store.CommitReport, error) {
	var cr store.CommitReport
	var diffCov []byte
	var uploadID *int64
	err := row.Scan(&cr.ID, &cr.RepoID, &cr.CommitSHA, &cr.Branch, &cr.PRID, &cr.TotalPct,
		&cr.CoveredStmts, &cr.TotalStmts, &cr.GateFailed, &diffCov, &cr.PartCount, &uploadID, &cr.CreatedAt, &cr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if uploadID != nil {
		cr.UploadID = *uploadID
	}
	if len(diffCov) > 0 {
		if err := json.Unmarshal(diffCov, &cr.DiffCoverage); err != nil {
			return nil, fmt.Errorf("commit report %d: bad diff_coverage: %w", cr.ID, err)
		}
	}
	return &cr, nil
}

// ensure interface compliance
var _ store.Store = (*Store)(nil)
