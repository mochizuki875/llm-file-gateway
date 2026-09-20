package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreScopesFilesByTenantAndLifetime(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(2_000_000_000, 0)
	store.now = func() time.Time { return now }

	active := testFile("file_active", "tenant-a", now.Unix(), now.Add(time.Hour).Unix())
	expired := testFile("file_expired", "tenant-a", now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix())
	for _, file := range []File{active, expired} {
		if err := store.Add(context.Background(), file); err != nil {
			t.Fatal(err)
		}
	}

	if file, err := store.Get(context.Background(), active.ID, "tenant-b"); err != nil || file != nil {
		t.Fatalf("cross-tenant get = %#v, %v; want nil, nil", file, err)
	}
	if file, err := store.Get(context.Background(), expired.ID, "tenant-a"); err != nil || file != nil {
		t.Fatalf("expired get = %#v, %v; want nil, nil", file, err)
	}
	files, err := store.List(context.Background(), "tenant-a", 20, "desc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != active.ID {
		t.Fatalf("listed files = %#v, want active file", files)
	}

	deleted, err := store.Delete(context.Background(), active.ID, "tenant-a")
	if err != nil || !deleted {
		t.Fatalf("delete = %t, %v", deleted, err)
	}
	if file, err := store.Get(context.Background(), active.ID, "tenant-a"); err != nil || file != nil {
		t.Fatalf("deleted get = %#v, %v; want nil, nil", file, err)
	}
	if file, err := store.GetInternal(context.Background(), active.ID); err != nil || file != nil {
		t.Fatalf("deleted internal get = %#v, %v; want nil, nil", file, err)
	}
	removed, err := store.Delete(context.Background(), expired.ID, expired.TenantID)
	if err != nil || !removed {
		t.Fatalf("delete expired = %t, %v; want true, nil", removed, err)
	}
	expiredFiles, err := store.Expired(context.Background())
	if err != nil || len(expiredFiles) != 0 {
		t.Fatalf("expired files after deletion = %#v, %v; want empty", expiredFiles, err)
	}
	if file, err := store.GetInternal(context.Background(), expired.ID); err != nil || file != nil {
		t.Fatalf("expired internal get = %#v, %v; want nil, nil", file, err)
	}
}

func TestPendingAndStatusUpdate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(2_000_000_000, 0)
	store.now = func() time.Time { return now }
	file := testFile("file_pending", "tenant-a", now.Unix(), now.Add(time.Hour).Unix())
	if err := store.Add(context.Background(), file); err != nil {
		t.Fatal(err)
	}

	ids, err := store.Pending(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != file.ID {
		t.Fatalf("pending = %v, %v", ids, err)
	}
	if err := store.UpdateStatus(context.Background(), file.ID, "processed", "files/manifest.json", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(context.Background(), file.ID, file.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "processed" || updated.ManifestPath.String != "files/manifest.json" {
		t.Fatalf("updated file = %#v", updated)
	}
}

func TestExpiredIncludesLegacyDeletedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	file := testFile("file_deleted", "tenant-a", now, now+3600)
	file.DeletedAt = sql.NullInt64{Int64: now, Valid: true}
	if err := dataStore.Add(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	files, err := dataStore.Expired(context.Background())
	if err != nil || len(files) != 1 || files[0].ID != file.ID {
		t.Fatalf("cleanup candidates = %#v, %v; want %s", files, err, file.ID)
	}
}

func testFile(id, tenant string, createdAt, expiresAt int64) File {
	return File{
		ID: id, TenantID: tenant, Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "hash", Status: "uploaded",
		SourcePath: "files/source.txt", CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
}
