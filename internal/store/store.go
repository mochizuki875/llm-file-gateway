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

type Store struct {
	database *sql.DB
	now      func() time.Time
}

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

func OpenDataDir(dataDir string) (*Store, error) {
	return Open(filepath.Join(dataDir, "gateway.db"))
}

func (store *Store) Close() error {
	return store.database.Close()
}

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
CREATE INDEX IF NOT EXISTS files_status ON files (status);
CREATE INDEX IF NOT EXISTS files_expires_at ON files (expires_at);`
	if _, err := store.database.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create database schema: %w", err)
	}
	return nil
}

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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(row rowScanner) (File, error) {
	var file File
	err := row.Scan(
		&file.ID, &file.TenantID, &file.Filename, &file.MediaType, &file.Purpose,
		&file.Bytes, &file.SHA256, &file.Status, &file.SourcePath, &file.ManifestPath,
		&file.ErrorMessage, &file.CreatedAt, &file.ExpiresAt, &file.DeletedAt,
	)
	return file, err
}

func (store *Store) List(ctx context.Context, tenantID string, limit int, order, after string) ([]File, error) {
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
	if after != "" {
		var cursorCreatedAt int64
		err := store.database.QueryRowContext(ctx,
			"SELECT created_at FROM files WHERE id = ? AND tenant_id = ?", after, tenantID,
		).Scan(&cursorCreatedAt)
		if err == nil {
			query += " AND created_at " + comparison + " ?"
			arguments = append(arguments, cursorCreatedAt)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get list cursor: %w", err)
		}
	}
	query += " ORDER BY created_at " + direction + " LIMIT ?"
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

func (store *Store) UpdateStatus(ctx context.Context, id, status, manifestPath, errorMessage string) error {
	_, err := store.database.ExecContext(ctx,
		`UPDATE files SET status = ?, manifest_path = NULLIF(?, ''), error_message = NULLIF(?, '') WHERE id = ?`,
		status, manifestPath, errorMessage, id,
	)
	if err != nil {
		return fmt.Errorf("update file status: %w", err)
	}
	return nil
}

func (store *Store) MarkDeleted(ctx context.Context, id, tenantID string) (*File, error) {
	file, err := store.Get(ctx, id, tenantID)
	if err != nil || file == nil {
		return file, err
	}
	result, err := store.database.ExecContext(ctx,
		"UPDATE files SET deleted_at = ? WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL",
		store.now().Unix(), id, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("mark file deleted: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return nil, err
	}
	return file, nil
}

func (store *Store) Pending(ctx context.Context) ([]string, error) {
	return store.selectIDs(ctx,
		"SELECT id FROM files WHERE status IN ('uploaded', 'processing') AND deleted_at IS NULL AND expires_at > ?",
		store.now().Unix(),
	)
}

func (store *Store) Expired(ctx context.Context) ([]File, error) {
	const query = `SELECT id, tenant_id, filename, media_type, purpose, byte_size, sha256,
status, source_path, manifest_path, error_message, created_at, expires_at, deleted_at
FROM files WHERE deleted_at IS NULL AND expires_at <= ?`
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
