package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// File is a single uploaded file record stored in the SQLite database.
type File struct {
	ID           string
	TenantID     string
	Filename     string
	MediaType    string
	Purpose      string
	Bytes        int64
	SHA256       string
	Status       string
	SourcePath   string
	ManifestPath sql.NullString
	ErrorMessage sql.NullString
	CreatedAt    int64
	ExpiresAt    int64
	DeletedAt    sql.NullInt64
}

// Store provides tenant-aware CRUD access to the SQLite file database.
type Store struct {
	database *sql.DB
	now      func() time.Time
}

// SharedTenantID is the tenant used for all files when gateway authentication
// is disabled.
const SharedTenantID = "shared"

// Open opens (or creates) the SQLite database at path, applies the schema
// migration, and returns a ready-to-use Store.
func Open(path string) (*Store, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("configure sqlite database: %w", err)
	}
	result := &Store{database: database, now: time.Now}
	if err := result.Migrate(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return result, nil
}

// OpenDataDir opens the gateway database inside the given data directory.
func OpenDataDir(dataDir string) (*Store, error) {
	return Open(filepath.Join(dataDir, "gateway.db"))
}

// Close closes the underlying database connection.
func (store *Store) Close() error {
	return store.database.Close()
}

// ConsolidateTenants moves every file into the shared tenant. It is used when
// gateway authentication is disabled so that all files are visible to all
// clients.
func (store *Store) ConsolidateTenants(ctx context.Context) error {
	if _, err := store.database.ExecContext(ctx, "UPDATE files SET tenant_id = ? WHERE tenant_id <> ?", SharedTenantID, SharedTenantID); err != nil {
		return fmt.Errorf("consolidate file tenants: %w", err)
	}
	return nil
}

// Migrate creates the files table and its indexes if they do not exist yet.
func (store *Store) Migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS files (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    filename TEXT NOT NULL,
    media_type TEXT NOT NULL,
    purpose TEXT NOT NULL,
    byte_size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    status TEXT NOT NULL,
    source_path TEXT NOT NULL,
    manifest_path TEXT,
    error_message TEXT,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS files_tenant_created ON files (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS files_tenant_created_id ON files (tenant_id, created_at, id);
CREATE INDEX IF NOT EXISTS files_status ON files (status);
CREATE INDEX IF NOT EXISTS files_expires_at ON files (expires_at);`
	if _, err := store.database.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create database schema: %w", err)
	}
	return nil
}

// Add inserts a new file record into the database.
func (store *Store) Add(ctx context.Context, file File) error {
	const statement = `INSERT INTO files (
id, tenant_id, filename, media_type, purpose, byte_size, sha256, status,
source_path, manifest_path, error_message, created_at, expires_at, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := store.database.ExecContext(ctx, statement,
		file.ID, file.TenantID, file.Filename, file.MediaType, file.Purpose,
		file.Bytes, file.SHA256, file.Status, file.SourcePath, file.ManifestPath,
		file.ErrorMessage, file.CreatedAt, file.ExpiresAt, file.DeletedAt,
	)
	if err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	return nil
}

// Get returns the non-deleted, non-expired file owned by the given tenant,
// or nil when no such file exists.
func (store *Store) Get(ctx context.Context, id, tenantID string) (*File, error) {
	const query = `SELECT id, tenant_id, filename, media_type, purpose, byte_size, sha256,
status, source_path, manifest_path, error_message, created_at, expires_at, deleted_at
FROM files WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL AND expires_at > ?`
	file, err := scanFile(store.database.QueryRowContext(ctx, query, id, tenantID, store.now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	return &file, nil
}

// GetInternal returns a file by ID regardless of tenant, expiry, or deletion
// status. It is used by the conversion workers.
func (store *Store) GetInternal(ctx context.Context, id string) (*File, error) {
	const query = `SELECT id, tenant_id, filename, media_type, purpose, byte_size, sha256,
status, source_path, manifest_path, error_message, created_at, expires_at, deleted_at
FROM files WHERE id = ?`
	file, err := scanFile(store.database.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get internal file: %w", err)
	}
	return &file, nil
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanFile can be shared.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanFile scans one database row into a File struct.
func scanFile(row rowScanner) (File, error) {
	var file File
	err := row.Scan(
		&file.ID, &file.TenantID, &file.Filename, &file.MediaType, &file.Purpose,
		&file.Bytes, &file.SHA256, &file.Status, &file.SourcePath, &file.ManifestPath,
		&file.ErrorMessage, &file.CreatedAt, &file.ExpiresAt, &file.DeletedAt,
	)
	return file, err
}

// List returns up to limit files for the given tenant, optionally filtered by
// purpose and ordered by created_at (asc or desc) with id as a tie-breaker.
// When after is non-empty, only files strictly after (or before, for desc)
// the cursor file in that total order are returned.
func (store *Store) List(ctx context.Context, tenantID, purpose string, limit int, order, after string) ([]File, error) {
	direction := "DESC"
	comparison := "<"
	if order == "asc" {
		direction = "ASC"
		comparison = ">"
	}
	arguments := []any{tenantID, store.now().Unix()}
	query := `SELECT id, tenant_id, filename, media_type, purpose, byte_size, sha256,
status, source_path, manifest_path, error_message, created_at, expires_at, deleted_at
FROM files WHERE tenant_id = ? AND deleted_at IS NULL AND expires_at > ?`
	if purpose != "" {
		query += " AND purpose = ?"
		arguments = append(arguments, purpose)
	}
	if after != "" {
		var cursorCreatedAt int64
		var cursorID string
		err := store.database.QueryRowContext(ctx,
			"SELECT created_at, id FROM files WHERE id = ? AND tenant_id = ?", after, tenantID,
		).Scan(&cursorCreatedAt, &cursorID)
		if err == nil {
			query += " AND (created_at " + comparison + " ? OR (created_at = ? AND id " + comparison + " ?))"
			arguments = append(arguments, cursorCreatedAt, cursorCreatedAt, cursorID)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get list cursor: %w", err)
		}
	}
	query += " ORDER BY created_at " + direction + ", id " + direction + " LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := store.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	files := make([]File, 0)
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan listed file: %w", err)
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// UpdateStatus updates the processing status of a file, optionally recording
// the manifest path or an error message. It only updates rows that have not
// been logically deleted and reports whether a row was actually updated so
// callers can detect files that were deleted concurrently.
func (store *Store) UpdateStatus(ctx context.Context, id, status, manifestPath, errorMessage string) (bool, error) {
	result, err := store.database.ExecContext(ctx,
		`UPDATE files SET status = ?, manifest_path = NULLIF(?, ''), error_message = NULLIF(?, '') WHERE id = ? AND deleted_at IS NULL`,
		status, manifestPath, errorMessage, id,
	)
	if err != nil {
		return false, fmt.Errorf("update file status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update file status rows: %w", err)
	}
	return count == 1, nil
}

// MarkDeleted logically deletes a file owned by the given tenant by setting
// deleted_at. It reports whether a row was actually marked so callers can
// detect files that were already deleted or never existed.
func (store *Store) MarkDeleted(ctx context.Context, id, tenantID string) (bool, error) {
	result, err := store.database.ExecContext(ctx,
		"UPDATE files SET deleted_at = ? WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL",
		store.now().Unix(), id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("mark file deleted: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark file deleted rows: %w", err)
	}
	return count == 1, nil
}

// Delete removes a file owned by the given tenant and reports whether a row
// was actually deleted.
func (store *Store) Delete(ctx context.Context, id, tenantID string) (bool, error) {
	result, err := store.database.ExecContext(ctx,
		"DELETE FROM files WHERE id = ? AND tenant_id = ?",
		id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("delete file: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// Pending retrieves the IDs of files that are either uploaded or processing and have not expired or been deleted.
func (store *Store) Pending(ctx context.Context) ([]string, error) {
	return store.selectIDs(ctx,
		"SELECT id FROM files WHERE status IN ('uploaded', 'processing') AND deleted_at IS NULL AND expires_at > ?",
		store.now().Unix(),
	)
}

// Expired returns all files that are deleted or past their expiry time.
func (store *Store) Expired(ctx context.Context) ([]File, error) {
	const query = `SELECT id, tenant_id, filename, media_type, purpose, byte_size, sha256,
status, source_path, manifest_path, error_message, created_at, expires_at, deleted_at
FROM files WHERE deleted_at IS NOT NULL OR expires_at <= ?`
	rows, err := store.database.QueryContext(ctx, query, store.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list expired files: %w", err)
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// selectIDs runs a query that returns a single id column and collects the
// results into a slice.
func (store *Store) selectIDs(ctx context.Context, query string, arguments ...any) ([]string, error) {
	rows, err := store.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
