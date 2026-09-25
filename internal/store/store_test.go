package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	files, err := store.List(context.Background(), "tenant-a", "", 20, "desc", "")
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

func TestMarkDeletedAndUpdateStatusSkipsDeleted(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(2_000_000_000, 0)
	store.now = func() time.Time { return now }
	file := testFile("file_mark", "tenant-a", now.Unix(), now.Add(time.Hour).Unix())
	if err := store.Add(context.Background(), file); err != nil {
		t.Fatal(err)
	}

	marked, err := store.MarkDeleted(context.Background(), file.ID, file.TenantID)
	if err != nil || !marked {
		t.Fatalf("mark deleted = %t, %v; want true, nil", marked, err)
	}
	// Marking again reports false (already deleted).
	marked, err = store.MarkDeleted(context.Background(), file.ID, file.TenantID)
	if err != nil || marked {
		t.Fatalf("second mark deleted = %t, %v; want false, nil", marked, err)
	}
	// A logically deleted file is hidden from the public API.
	if current, err := store.Get(context.Background(), file.ID, file.TenantID); err != nil || current != nil {
		t.Fatalf("get after mark = %#v, %v; want nil, nil", current, err)
	}
	// Status updates must not resurrect a logically deleted file.
	updated, err := store.UpdateStatus(context.Background(), file.ID, "processed", "files/manifest.json", "")
	if err != nil || updated {
		t.Fatalf("update status after mark = %t, %v; want false, nil", updated, err)
	}
	current, err := store.GetInternal(context.Background(), file.ID)
	if err != nil || current == nil {
		t.Fatalf("internal get = %#v, %v", current, err)
	}
	if current.Status != "uploaded" || !current.DeletedAt.Valid {
		t.Fatalf("record after update = %#v; want unchanged uploaded + deleted_at", current)
	}
	// The record is still reported as expired for physical cleanup.
	expired, err := store.Expired(context.Background())
	if err != nil || len(expired) != 1 || expired[0].ID != file.ID {
		t.Fatalf("expired = %#v, %v; want the marked file", expired, err)
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
	if updated, err := store.UpdateStatus(context.Background(), file.ID, "processed", "files/manifest.json", ""); err != nil || !updated {
		t.Fatalf("update status = %t, %v", updated, err)
	}
	updated, err := store.Get(context.Background(), file.ID, file.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "processed" || updated.ManifestPath.String != "files/manifest.json" {
		t.Fatalf("updated file = %#v", updated)
	}
}

func TestConsolidateTenants(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	now := time.Now().Unix()
	for index, tenantID := range []string{"tenant-a", "tenant-b"} {
		file := testFile(fmt.Sprintf("file_%d", index), tenantID, now, now+3600)
		if err := dataStore.Add(context.Background(), file); err != nil {
			t.Fatal(err)
		}
	}
	if err := dataStore.ConsolidateTenants(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := dataStore.List(context.Background(), SharedTenantID, "", 20, "desc", "")
	if err != nil || len(files) != 2 {
		t.Fatalf("shared files = %#v, %v; want 2 files", files, err)
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

func TestListPaginationWithSameCreatedAt(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	now := time.Unix(2_000_000_000, 0)
	dataStore.now = func() time.Time { return now }

	// 30 files share the exact same created_at; ids are generated in a
	// deterministic order so the expected total order is known.
	const total = 30
	for index := 0; index < total; index++ {
		file := testFile(fmt.Sprintf("file_%02d", index), "tenant-a", now.Unix(), now.Add(time.Hour).Unix())
		file.Purpose = "user_data"
		if index%2 == 0 {
			file.Purpose = "assistants"
		}
		if err := dataStore.Add(context.Background(), file); err != nil {
			t.Fatal(err)
		}
	}

	// Walk every page in both directions and verify no file is lost or
	// duplicated, including with a purpose filter.
	for _, test := range []struct {
		name    string
		order   string
		purpose string
	}{
		{name: "desc_all", order: "desc"},
		{name: "asc_all", order: "asc"},
		{name: "desc_purpose", order: "desc", purpose: "assistants"},
		{name: "asc_purpose", order: "asc", purpose: "assistants"},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := make(map[string]bool)
			after := ""
			for page := 0; ; page++ {
				records, err := dataStore.List(context.Background(), "tenant-a", test.purpose, 7, test.order, after)
				if err != nil {
					t.Fatal(err)
				}
				if len(records) == 0 {
					break
				}
				if len(records) > 7 {
					t.Fatalf("page %d returned %d records, want at most 7", page, len(records))
				}
				for _, record := range records {
					if seen[record.ID] {
						t.Fatalf("page %d returned duplicate file %s", page, record.ID)
					}
					seen[record.ID] = true
				}
				after = records[len(records)-1].ID
				if page > total {
					t.Fatal("pagination did not terminate")
				}
			}
			want := 0
			for index := 0; index < total; index++ {
				if test.purpose == "" || index%2 == 0 {
					want++
				}
			}
			if len(seen) != want {
				t.Fatalf("walked %d files, want %d", len(seen), want)
			}
		})
	}
}

func TestOpenDataDir(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := OpenDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	if _, err := os.Stat(filepath.Join(dataDir, "gateway.db")); err != nil {
		t.Fatalf("gateway.db = %v", err)
	}
	// The database must be usable after opening through the data directory.
	now := time.Now().Unix()
	file := testFile("file_datadir", "tenant-a", now, now+3600)
	if err := dataStore.Add(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	got, err := dataStore.Get(context.Background(), file.ID, file.TenantID)
	if err != nil || got == nil || got.ID != file.ID {
		t.Fatalf("get = %#v, %v", got, err)
	}
}

func TestGetInternal(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	now := time.Now().Unix()
	file := testFile("file_internal", "tenant-a", now, now+3600)
	if err := dataStore.Add(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	got, err := dataStore.GetInternal(context.Background(), file.ID)
	if err != nil || got == nil {
		t.Fatalf("get internal = %#v, %v", got, err)
	}
	if got.ID != file.ID || got.TenantID != file.TenantID || got.Status != file.Status {
		t.Fatalf("internal file = %#v", got)
	}
	// Missing IDs return nil without an error.
	missing, err := dataStore.GetInternal(context.Background(), "file_missing")
	if err != nil || missing != nil {
		t.Fatalf("missing internal = %#v, %v; want nil, nil", missing, err)
	}
}

func TestOpenRejectsInvalidPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "gateway.db")); err == nil {
		t.Fatal("Open() succeeded for a missing directory")
	}
}

func testFile(id, tenant string, createdAt, expiresAt int64) File {
	return File{
		ID: id, TenantID: tenant, Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "hash", Status: "uploaded",
		SourcePath: "files/source.txt", CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
}
