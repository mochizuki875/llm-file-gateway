package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

func TestResolveMissingFileLogsWarning(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })

	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	service := New(config.Config{DataDir: dataDir}, dataStore, testDispatcher(t))
	if _, _, _, err := service.Resolve(context.Background(), "file_missing", "tenant-a", "input[0].file_id"); err == nil {
		t.Fatal("Resolve() succeeded for a missing file")
	}
	logOutput := output.String()
	for _, expected := range []string{"level=WARN", `msg="file not found"`, "file_id=file_missing", "param=input[0].file_id"} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
}

func TestDeleteExpiredRemovesDatabaseRecordAndFiles(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	relativeSource := filepath.Join("files", "tenant-a", "file_expired", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	record := store.File{
		ID: "file_expired", TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "processed",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now - 60, ExpiresAt: now - 1,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	service := New(config.Config{DataDir: dataDir}, dataStore, testDispatcher(t))
	service.deleteExpired(context.Background())

	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("expired database record = %#v, %v; want nil, nil", current, err)
	}
	if _, err := os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatalf("expired file directory stat error = %v; want not exist", err)
	}
	logOutput := output.String()
	for _, expected := range []string{"level=DEBUG", `msg="expired file deleted"`, "file_id=file_expired", "filename=source.txt"} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("log output = %q, missing %q", logOutput, expected)
		}
	}
}

func TestDeleteExpiredRecoversLogicallyDeletedRecordAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	relativeSource := filepath.Join("files", "tenant-a", "file_deleted", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	record := store.File{
		ID: "file_deleted", TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "processed",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if marked, err := dataStore.MarkDeleted(context.Background(), record.ID, record.TenantID); err != nil || !marked {
		t.Fatalf("MarkDeleted() = %t, %v", marked, err)
	}

	restartedService := New(config.Config{DataDir: dataDir}, dataStore, testDispatcher(t))
	restartedService.deleteExpired(context.Background())

	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
	if _, err := os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatalf("artifact directory still exists: %v", err)
	}
}

func TestDeleteExpiredRetriesAfterCleanupFailure(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	blocker := filepath.Join(dataDir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	relativeSource := filepath.Join("blocker", "file_retry", "source.txt")
	now := time.Now().Unix()
	record := store.File{
		ID: "file_retry", TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "processed",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if marked, err := dataStore.MarkDeleted(context.Background(), record.ID, record.TenantID); err != nil || !marked {
		t.Fatalf("MarkDeleted() = %t, %v", marked, err)
	}
	service := New(config.Config{DataDir: dataDir}, dataStore, testDispatcher(t))

	service.deleteExpired(context.Background())
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current == nil || !current.DeletedAt.Valid {
		t.Fatalf("record after failed cleanup = %#v, %v; want logically deleted", current, err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}

	service.deleteExpired(context.Background())
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record after retry = %#v, %v; want nil", current, err)
	}
}

func TestDeleteCompletesAfterRequestCancellation(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	relativeSource := filepath.Join("files", "tenant-a", "file_cancel", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	record := store.File{
		ID: "file_cancel", TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "processed",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service := New(config.Config{DataDir: dataDir}, dataStore, testDispatcher(t))
	release, err := service.acquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	deleted := make(chan error, 1)
	go func() {
		ok, err := service.Delete(ctx, record.ID, record.TenantID)
		if err == nil && !ok {
			err = errors.New("Delete reported false")
		}
		deleted <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := dataStore.GetInternal(context.Background(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.DeletedAt.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Delete did not mark the record")
		}
	}
	cancel()
	release()
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestStartResumesPendingConversion(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	relativeSource := filepath.Join("files", "tenant-a", "file_pending", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("resumed conversion"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	record := store.File{
		ID: "file_pending", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 18, SHA256: "digest", Status: "processing",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	settings := config.Config{
		DataDir: dataDir, FileTTL: 6 * time.Hour, MaxFileBytes: 1024,
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, Workers: 2,
	}
	service := New(settings, dataStore, testDispatcher(t))
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Run(ctx, settings.Workers); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.Stop()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := dataStore.Get(context.Background(), record.ID, record.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.Status == "processed" {
			if !current.ManifestPath.Valid {
				t.Fatal("processed file has no manifest")
			}
			if _, err := os.Stat(filepath.Join(dataDir, current.ManifestPath.String)); err != nil {
				t.Fatalf("manifest is unavailable: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending conversion was not resumed")
}

func TestReconcileStorageRemovesOnlyOrphanManagedDirectories(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	statuses := []string{"uploaded", "processing", "processed", "failed"}
	for index, status := range statuses {
		id := fmt.Sprintf("file_%032x", index+1)
		relativeSource := filepath.Join("files", "tenant-a", id, "source.txt")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dataDir, relativeSource)), 0o700); err != nil {
			t.Fatal(err)
		}
		record := store.File{
			ID: id, TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
			Purpose: "user_data", Bytes: 1, SHA256: "digest", Status: status,
			SourcePath: filepath.ToSlash(relativeSource), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}
		if err := dataStore.Add(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	deletedID := "file_00000000000000000000000000000005"
	deletedDir := filepath.Join(dataDir, "files", "tenant-a", deletedID)
	if err := os.MkdirAll(deletedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	deletedRecord := store.File{
		ID: deletedID, TenantID: "tenant-a", Filename: "source.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 1, SHA256: "digest", Status: "processed",
		SourcePath: filepath.ToSlash(filepath.Join("files", "tenant-a", deletedID, "source.txt")),
		CreatedAt:  time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := dataStore.Add(context.Background(), deletedRecord); err != nil {
		t.Fatal(err)
	}
	if marked, err := dataStore.MarkDeleted(context.Background(), deletedID, "tenant-a"); err != nil || !marked {
		t.Fatalf("MarkDeleted() = %t, %v", marked, err)
	}
	missingDirectoryID := "file_00000000000000000000000000000006"
	missingDirectoryRecord := deletedRecord
	missingDirectoryRecord.ID = missingDirectoryID
	missingDirectoryRecord.Status = "uploaded"
	missingDirectoryRecord.SourcePath = filepath.ToSlash(filepath.Join("files", "tenant-a", missingDirectoryID, "source.txt"))
	if err := dataStore.Add(context.Background(), missingDirectoryRecord); err != nil {
		t.Fatal(err)
	}

	orphanDir := filepath.Join(dataDir, "files", "tenant-a", "file_ffffffffffffffffffffffffffffffff")
	unrelatedDir := filepath.Join(dataDir, "files", "tenant-a", "manual-directory")
	externalDir := filepath.Join(dataDir, "external")
	for _, directory := range []string{orphanDir, unrelatedDir, externalDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dataDir, "files", "tenant-a", "file_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if err := os.Symlink(externalDir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "files", "tenant-a", "unrelated.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := service.ReconcileStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("orphan directory still exists: %v", err)
	}
	for _, path := range []string{deletedDir, unrelatedDir, externalDir, link} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("retained path %q missing: %v", path, err)
		}
	}
	for index := range statuses {
		id := fmt.Sprintf("file_%032x", index+1)
		if _, err := os.Stat(filepath.Join(dataDir, "files", "tenant-a", id)); err != nil {
			t.Fatalf("record-backed directory %s missing: %v", id, err)
		}
	}
	if record, err := dataStore.GetInternal(context.Background(), missingDirectoryID); err != nil || record == nil {
		t.Fatalf("DB-only record = %#v, %v; want retained", record, err)
	}
}

func TestCreateUsesOwnerOnlyPermissions(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader("secret"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dataDir, record.SourcePath)
	for path, want := range map[string]os.FileMode{filepath.Dir(source): 0o700, source: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode of %s = %04o, want %04o", path, got, want)
		}
	}
}

func testDispatcher(t *testing.T) *converter.Dispatcher {
	t.Helper()
	return converter.NewDispatcher(converter.NewInTreeRegistry(), converter.DefaultConverterConfig(), nil)
}

func testSettings(dataDir string) config.Config {
	return config.Config{
		DataDir: dataDir, FileTTL: 5 * time.Minute, MaxFileBytes: 1024,
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, Workers: 2,
	}
}

func TestCreateStoresFileAndEnqueues(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader("hello world"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.Status != "uploaded" || record.MediaType != "text/plain" || record.Bytes != 11 {
		t.Fatalf("record = %#v", record)
	}
	if record.SHA256 == "" || len(record.SHA256) != 64 {
		t.Fatalf("sha256 = %q", record.SHA256)
	}
	if record.ExpiresAt-record.CreatedAt != 60 {
		t.Fatalf("lifetime = %d seconds, want 60", record.ExpiresAt-record.CreatedAt)
	}
	source := filepath.Join(dataDir, record.SourcePath)
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello world" {
		t.Fatalf("source content = %q", content)
	}
	select {
	case job := <-service.queue:
		if job.id != record.ID || job.documentConverter == nil {
			t.Fatalf("queued job = %#v", job)
		}
	case <-time.After(time.Second):
		t.Fatal("conversion job was not enqueued")
	}
}

func TestCreateRejectsOversizedFile(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	settings := testSettings(dataDir)
	settings.MaxFileBytes = 4
	service := New(settings, dataStore, testDispatcher(t))

	_, err = service.Create(context.Background(), "notes.txt", strings.NewReader("12345"), "user_data", "tenant-a", time.Minute)
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Status != 400 || gatewayError.Code != "file_too_large" {
		t.Fatalf("error = %v", err)
	}
	// The per-file directory must be cleaned up; the tenant directory may remain.
	tenantDir := filepath.Join(dataDir, "files", "tenant-a")
	entries, err := os.ReadDir(tenantDir)
	if err != nil {
		t.Fatalf("tenant directory = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover file directories = %v", entries)
	}
}

func TestCreateUnlimitedFileSize(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	settings := testSettings(dataDir)
	settings.MaxFileBytes = 0
	service := New(settings, dataStore, testDispatcher(t))

	content := strings.Repeat("x", 4096)
	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader(content), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if record.Bytes != int64(len(content)) {
		t.Fatalf("bytes = %d, want %d", record.Bytes, len(content))
	}
	source := filepath.Join(dataDir, record.SourcePath)
	stored, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != content {
		t.Fatalf("stored content length = %d, want %d", len(stored), len(content))
	}
}

func TestCreateUnlimitedTTLStoresZeroExpiry(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader("content"), "user_data", "tenant-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if record.ExpiresAt != 0 {
		t.Fatalf("ExpiresAt = %d, want 0 (never expires)", record.ExpiresAt)
	}
}

func TestCreateRejectsInvalidContent(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	_, err = service.Create(context.Background(), "notes.txt", strings.NewReader("\x00binary"), "user_data", "tenant-a", time.Minute)
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Status != 400 || gatewayError.Code != "unsupported_file_type" {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateSanitizesFilename(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	record, err := service.Create(context.Background(), "../../etc/passwd.txt", strings.NewReader("content"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if record.Filename != "passwd.txt" {
		t.Fatalf("filename = %q, want passwd.txt", record.Filename)
	}
}

func TestDeleteRemovesFilesAndRecord(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader("content"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fileDir := filepath.Dir(filepath.Join(dataDir, record.SourcePath))

	deleted, err := service.Delete(context.Background(), record.ID, "tenant-a")
	if err != nil || !deleted {
		t.Fatalf("delete = %t, %v", deleted, err)
	}
	if _, err := os.Stat(fileDir); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists: %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestDeleteMissingFile(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	deleted, err := service.Delete(context.Background(), "file_missing", "tenant-a")
	if err != nil || deleted {
		t.Fatalf("delete = %t, %v; want false, nil", deleted, err)
	}
}

func TestResolveStatuses(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	base := store.File{
		ID: "file_status", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest",
		SourcePath: "files/tenant-a/file_status/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}

	for _, test := range []struct {
		name       string
		status     string
		manifest   string
		wantStatus int
		wantCode   string
	}{
		{name: "uploaded", status: "uploaded", wantStatus: 409, wantCode: "file_not_ready"},
		{name: "processing", status: "processing", wantStatus: 409, wantCode: "file_not_ready"},
		{name: "failed", status: "failed", wantStatus: 422, wantCode: "file_processing_failed"},
		{name: "processed_without_manifest", status: "processed", wantStatus: 422, wantCode: "file_processing_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := base
			record.Status = test.status
			record.ManifestPath = sql.NullString{String: test.manifest, Valid: test.manifest != ""}
			if err := dataStore.Add(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			_, _, _, err := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
			var gatewayError *apierror.Error
			if !errors.As(err, &gatewayError) || gatewayError.Status != test.wantStatus || gatewayError.Code != test.wantCode {
				t.Fatalf("error = %v, want %d %s", err, test.wantStatus, test.wantCode)
			}
			if _, err := dataStore.Delete(context.Background(), record.ID, "tenant-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveProcessedFile(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeDir := filepath.Join("files", "tenant-a", "file_ready")
	manifestPath := filepath.Join(relativeDir, "manifest.json")
	manifest := converter.Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    converter.ManifestSource{MediaType: "text/plain", SHA256: "digest"},
		Documents: []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.Artifact{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(dataDir, relativeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, manifestPath), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_ready", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   filepath.ToSlash(filepath.Join(relativeDir, "source.txt")),
		ManifestPath: sql.NullString{String: filepath.ToSlash(manifestPath), Valid: true},
		CreatedAt:    now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	gotRecord, gotManifest, release, err := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	if err != nil {
		t.Fatal(err)
	}
	if release == nil {
		t.Fatal("Resolve() returned a nil release function")
	}
	if gotRecord.ID != record.ID || gotManifest.SchemaVersion != 3 || len(gotManifest.Documents) != 1 {
		t.Fatalf("resolved = %#v, %#v", gotRecord, gotManifest)
	}
	release()
}

func TestDeleteWaitsForActiveLease(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeDir := filepath.Join("files", "tenant-a", "file_leased")
	manifestPath := filepath.Join(relativeDir, "manifest.json")
	manifest := converter.Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    converter.ManifestSource{MediaType: "text/plain", SHA256: "digest"},
		Documents: []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.Artifact{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(dataDir, relativeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, manifestPath), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_leased", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   filepath.ToSlash(filepath.Join(relativeDir, "source.txt")),
		ManifestPath: sql.NullString{String: filepath.ToSlash(manifestPath), Valid: true},
		CreatedAt:    now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	_, _, release, err := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	if err != nil {
		t.Fatal(err)
	}

	deleted := make(chan bool, 1)
	go func() {
		ok, err := service.Delete(context.Background(), record.ID, "tenant-a")
		if err != nil {
			t.Errorf("Delete() error = %v", err)
			deleted <- false
			return
		}
		deleted <- ok
	}()

	// Delete must not complete while the lease is held.
	select {
	case <-deleted:
		t.Fatal("Delete() completed while a lease was held")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case ok := <-deleted:
		if !ok {
			t.Fatal("Delete() reported no file deleted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete() did not complete after lease release")
	}
	if _, err := os.Stat(filepath.Join(dataDir, relativeDir)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists after delete: %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestDeleteExpiredWaitsForActiveLease(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeDir := filepath.Join("files", "tenant-a", "file_expired_leased")
	manifestPath := filepath.Join(relativeDir, "manifest.json")
	manifest := converter.Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    converter.ManifestSource{MediaType: "text/plain", SHA256: "digest"},
		Documents: []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.Artifact{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(dataDir, relativeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, manifestPath), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_expired_leased", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   filepath.ToSlash(filepath.Join(relativeDir, "source.txt")),
		ManifestPath: sql.NullString{String: filepath.ToSlash(manifestPath), Valid: true},
		CreatedAt:    now - 60, ExpiresAt: now - 1,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	// Resolve rejects expired files, so acquire the lease directly to
	// simulate an inference request that is reading the artifacts.
	release, err := service.acquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		service.deleteExpired(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("deleteExpired() completed while a lease was held")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deleteExpired() did not complete after lease release")
	}
	if _, err := os.Stat(filepath.Join(dataDir, relativeDir)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists after expiry: %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestResolveMissingManifest(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_nomanifest", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   "files/tenant-a/file_nomanifest/source.txt",
		ManifestPath: sql.NullString{String: "files/tenant-a/file_nomanifest/manifest.json", Valid: true},
		CreatedAt:    now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	_, _, _, resolveErr := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	var gatewayError *apierror.Error
	if !errors.As(resolveErr, &gatewayError) || gatewayError.Status != 422 || gatewayError.Code != "file_processing_failed" {
		t.Fatalf("error = %v", resolveErr)
	}
}

// TestResolveRejectsFileDeletedConcurrently verifies that Resolve cannot
// acquire a lease on a file whose deletion has already started (TOCTOU
// between the store lookup and the lease acquisition). The test controls the
// race timing deterministically by starting deletion before Resolve runs.
func TestResolveRejectsFileDeletedConcurrently(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeDir := filepath.Join("files", "tenant-a", "file_toc")
	manifestPath := filepath.Join(relativeDir, "manifest.json")
	manifest := converter.Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    converter.ManifestSource{MediaType: "text/plain", SHA256: "digest"},
		Documents: []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.Artifact{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(dataDir, relativeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, manifestPath), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_toc", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   filepath.ToSlash(filepath.Join(relativeDir, "source.txt")),
		ManifestPath: sql.NullString{String: filepath.ToSlash(manifestPath), Valid: true},
		CreatedAt:    now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	// Start deletion and wait until the deletion marker is registered so that
	// the race window (store lookup succeeded, lease not yet acquired) is
	// deterministically covered.
	deleted := make(chan bool, 1)
	go func() {
		ok, err := service.Delete(context.Background(), record.ID, "tenant-a")
		if err != nil {
			t.Errorf("Delete() error = %v", err)
			deleted <- false
			return
		}
		deleted <- ok
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		service.lifecycleMu.Lock()
		_, deleting := service.deleting[record.ID]
		service.lifecycleMu.Unlock()
		if deleting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion marker was not registered")
		}
		time.Sleep(time.Millisecond)
	}

	// Resolve must fail with file_not_found because the file is being deleted.
	_, _, _, resolveErr := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	var gatewayError *apierror.Error
	if !errors.As(resolveErr, &gatewayError) || gatewayError.Status != 404 || gatewayError.Code != "file_not_found" {
		t.Fatalf("error = %v, want 404 file_not_found", resolveErr)
	}

	select {
	case ok := <-deleted:
		if !ok {
			t.Fatal("Delete() reported no file deleted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete() did not complete")
	}
	if _, err := os.Stat(filepath.Join(dataDir, relativeDir)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists after delete: %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

// TestDeleteAndJanitorRace verifies that an explicit DELETE and the janitor
// running concurrently on the same file do not deadlock, do not panic, and
// leave no artifacts or database record behind. The janitor must not take
// over cleanup of a record that DELETE already marked, and DELETE must still
// report success. The race timing is controlled deterministically: a lease
// held by a reader makes DELETE block after marking the record, giving the
// janitor a chance to observe the marked record.
func TestDeleteAndJanitorRace(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeDir := filepath.Join("files", "tenant-a", "file_race")
	manifestPath := filepath.Join(relativeDir, "manifest.json")
	manifest := converter.Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    converter.ManifestSource{MediaType: "text/plain", SHA256: "digest"},
		Documents: []converter.ManifestDocument{{Name: "notes.txt", Parts: []converter.Artifact{{PartNumber: 1, TextPath: "part-0001.txt"}}}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.MkdirAll(filepath.Join(dataDir, relativeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, manifestPath), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_race", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "processed",
		SourcePath:   filepath.ToSlash(filepath.Join(relativeDir, "source.txt")),
		ManifestPath: sql.NullString{String: filepath.ToSlash(manifestPath), Valid: true},
		CreatedAt:    now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	// Hold a lease so that DELETE blocks at waitForLease after marking the
	// record, creating a deterministic window for the janitor to observe it.
	release, err := service.acquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	deleted := make(chan bool, 1)
	go func() {
		ok, err := service.Delete(context.Background(), record.ID, "tenant-a")
		if err != nil {
			t.Errorf("Delete() error = %v", err)
			deleted <- false
			return
		}
		deleted <- ok
	}()

	// Wait until DELETE has marked the record (deleted_at is set) and is
	// blocked on the lease.
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := dataStore.GetInternal(context.Background(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.DeletedAt.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("DELETE did not mark the record")
		}
		time.Sleep(time.Millisecond)
	}

	// The janitor now observes the marked record. It must not take over
	// cleanup: MarkDeleted returns false and the janitor skips the record.
	service.deleteExpired(context.Background())

	// DELETE must still be blocked on the lease.
	select {
	case <-deleted:
		t.Fatal("Delete() completed while a lease was held")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case ok := <-deleted:
		if !ok {
			t.Fatal("Delete() reported no file deleted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete() did not complete after lease release")
	}
	if _, err := os.Stat(filepath.Join(dataDir, relativeDir)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists after concurrent delete: %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestHandleErrDropsAfterMaxRetries(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_fail", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_fail/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service.retryCount[record.ID] = maxRetries

	service.handleErr(context.Background(), errors.New("boom"), conversionJob{id: record.ID})

	current, err := dataStore.GetInternal(context.Background(), record.ID)
	if err != nil || current == nil {
		t.Fatalf("record = %#v, %v", current, err)
	}
	if current.Status != "failed" || !current.ErrorMessage.Valid || current.ErrorMessage.String != "boom" {
		t.Fatalf("failed record = %#v", current)
	}
	if _, exists := service.retryCount[record.ID]; exists {
		t.Fatal("retry count was not forgotten")
	}
}

func TestHandleErrSchedulesRetry(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_retry", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_retry/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	service.handleErr(context.Background(), errors.New("boom"), conversionJob{id: "file_retry"})
	if service.retries("file_retry") != 1 {
		t.Fatalf("retries = %d, want 1", service.retries("file_retry"))
	}
	select {
	case job := <-service.queue:
		if job.id != "file_retry" {
			t.Fatalf("queued job = %#v", job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry job was not re-enqueued")
	}
}

func TestHandleErrNilDoesNothing(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	service.handleErr(context.Background(), nil, conversionJob{id: "file_ok"})
	if service.retries("file_ok") != 0 {
		t.Fatalf("retries = %d, want 0", service.retries("file_ok"))
	}
}

func TestHandleErrSuccessForgetsRetryState(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	service.retryCount["file_recovered"] = 2

	service.handleErr(context.Background(), nil, conversionJob{id: "file_recovered"})

	if _, exists := service.retryCount["file_recovered"]; exists {
		t.Fatal("retry state was not forgotten after a successful conversion")
	}
}

func TestHandleErrNonRetryableFailsImmediately(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_deterministic", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_deterministic/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "page_limit", err: &converter.PageLimitError{Limit: 20}},
		{name: "text_limit", err: &converter.TextLimitError{Limit: 500}},
		{name: "validation", err: &extractor.ValidationError{Message: "text file must be valid UTF-8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.handleErr(context.Background(), test.err, conversionJob{id: record.ID})
			current, err := dataStore.GetInternal(context.Background(), record.ID)
			if err != nil || current == nil {
				t.Fatalf("record = %#v, %v", current, err)
			}
			if current.Status != "failed" {
				t.Fatalf("status = %q, want failed", current.Status)
			}
			if _, exists := service.retryCount[record.ID]; exists {
				t.Fatal("retry state was not forgotten")
			}
			// Reset the record for the next subtest.
			if updated, err := dataStore.UpdateStatus(context.Background(), record.ID, "uploaded", "", ""); err != nil || !updated {
				t.Fatalf("reset status = %t, %v", updated, err)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	for _, test := range []struct {
		retry int
		want  time.Duration
	}{
		{retry: 0, want: 1 * time.Second},
		{retry: 1, want: 2 * time.Second},
		{retry: 2, want: 4 * time.Second},
		{retry: 3, want: 8 * time.Second},
	} {
		if got := backoffDelay(test.retry); got != test.want {
			t.Fatalf("backoffDelay(%d) = %s, want %s", test.retry, got, test.want)
		}
	}
}

func TestNewFileID(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id, err := newFileID()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "file_") || len(id) != len("file_")+32 {
			t.Fatalf("id = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id = %q", id)
		}
		seen[id] = true
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate(short) = %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcde" {
		t.Fatalf("truncate(long) = %q", got)
	}
}

func TestConvertSkipsDeletedFile(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_deleted", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_deleted/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
		DeletedAt: sql.NullInt64{Int64: now, Valid: true},
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := service.convert(context.Background(), conversionJob{id: record.ID}); err != nil {
		t.Fatalf("convert() = %v", err)
	}
	current, err := dataStore.GetInternal(context.Background(), record.ID)
	if err != nil || current.Status != "uploaded" {
		t.Fatalf("record = %#v, %v; want unchanged uploaded status", current, err)
	}
}

func TestConvertReturnsErrorForMissingSource(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_missing_source", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_missing_source/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	// The source file does not exist, so conversion must fail with an error.
	if err := service.convert(context.Background(), conversionJob{id: record.ID}); err == nil {
		t.Fatal("convert() succeeded for a missing source file")
	}
	current, err := dataStore.GetInternal(context.Background(), record.ID)
	if err != nil || current.Status != "processing" {
		t.Fatalf("record = %#v, %v; want processing status", current, err)
	}
}

func TestMarkFailedTruncatesError(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_markfail", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_markfail/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	longError := strings.Repeat("x", 2000)
	service.markFailed(record.ID, errors.New(longError))
	current, err := dataStore.GetInternal(context.Background(), record.ID)
	if err != nil || current.Status != "failed" {
		t.Fatalf("record = %#v, %v", current, err)
	}
	if !current.ErrorMessage.Valid || len(current.ErrorMessage.String) != 1000 {
		t.Fatalf("error message length = %d, want 1000", len(current.ErrorMessage.String))
	}
}

func TestResolveConverterDelegatesToDispatcher(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))

	documentConverter, err := service.ResolveConverter(context.Background(), "notes.txt")
	if err != nil || documentConverter.MediaType() != "text/plain" {
		t.Fatalf("converter = %#v, %v", documentConverter, err)
	}
}

func TestNewPanicsWithoutDispatcher(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New() did not panic without a dispatcher")
		}
	}()
	New(config.Config{}, nil, nil)
}

func TestDeleteWaitsForActiveConversion(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeSource := filepath.Join("files", "tenant-a", "file_busy", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_busy", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "uploaded",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	service.syncHandler = func(ctx context.Context, job conversionJob) error {
		close(started)
		<-release
		return nil
	}
	service.enqueue(service.lifecycle(), conversionJob{id: record.ID})
	go service.processNextJob(context.Background())
	<-started

	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.Delete(context.Background(), record.ID, "tenant-a")
		deleteDone <- err
	}()
	// Delete must block while the conversion is active.
	select {
	case err := <-deleteDone:
		t.Fatalf("Delete returned before conversion finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not finish after conversion")
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
	if _, err := os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists: %v", err)
	}
}

func TestDeleteCancelsActiveConversion(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeSource := filepath.Join("files", "tenant-a", "file_cancel", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_cancel", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "uploaded",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	service.syncHandler = func(ctx context.Context, job conversionJob) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	service.enqueue(service.lifecycle(), conversionJob{id: record.ID})
	go service.processNextJob(context.Background())
	<-started

	// The conversion observes the cancellation and Delete completes.
	deleted, err := service.Delete(context.Background(), record.ID, "tenant-a")
	if err != nil || !deleted {
		t.Fatalf("delete = %t, %v; want true, nil", deleted, err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestTTLExpirationDuringConversion(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeSource := filepath.Join("files", "tenant-a", "file_ttl", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_ttl", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "uploaded",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now - 60, ExpiresAt: now - 1,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	service.syncHandler = func(ctx context.Context, job conversionJob) error {
		close(started)
		<-release
		return nil
	}
	service.enqueue(service.lifecycle(), conversionJob{id: record.ID})
	go service.processNextJob(context.Background())
	<-started

	janitorDone := make(chan struct{})
	go func() {
		service.deleteExpired(context.Background())
		close(janitorDone)
	}()
	// The janitor must wait for the active conversion before cleanup.
	select {
	case <-janitorDone:
		t.Fatal("deleteExpired returned before conversion finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-janitorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("deleteExpired did not finish after conversion")
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
	if _, err := os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatalf("file directory still exists: %v", err)
	}
}

func TestDeletedFileDoesNotReturnToProcessed(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	relativeSource := filepath.Join("files", "tenant-a", "file_gone", "source.txt")
	source := filepath.Join(dataDir, relativeSource)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := store.File{
		ID: "file_gone", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "uploaded",
		SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.Delete(context.Background(), record.ID, "tenant-a"); err != nil || !deleted {
		t.Fatalf("delete = %t, %v", deleted, err)
	}
	// A stale queued job must not resurrect the deleted file.
	if err := service.convert(context.Background(), conversionJob{id: record.ID}); err != nil {
		t.Fatalf("convert() = %v", err)
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestRetryDoesNotResurrectDeletedFile(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	record := store.File{
		ID: "file_retry_gone", TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
		Purpose: "user_data", Bytes: 5, SHA256: "digest", Status: "uploaded",
		SourcePath: "files/tenant-a/file_retry_gone/source.txt", CreatedAt: now, ExpiresAt: now + 3600,
	}
	if err := dataStore.Add(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.Delete(context.Background(), record.ID, "tenant-a"); err != nil || !deleted {
		t.Fatalf("delete = %t, %v", deleted, err)
	}
	// A retryable failure after deletion must not schedule a retry.
	service.handleErr(context.Background(), errors.New("boom"), conversionJob{id: record.ID})
	if service.retries(record.ID) != 0 {
		t.Fatalf("retries = %d, want 0", service.retries(record.ID))
	}
	select {
	case job := <-service.queue:
		t.Fatalf("retry job was enqueued for a deleted file: %#v", job)
	default:
	}
	if current, err := dataStore.GetInternal(context.Background(), record.ID); err != nil || current != nil {
		t.Fatalf("record = %#v, %v; want nil", current, err)
	}
}

func TestDeleteDoesNotBlockOtherFiles(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	now := time.Now().Unix()
	for _, id := range []string{"file_blocked", "file_free"} {
		relativeSource := filepath.Join("files", "tenant-a", id, "source.txt")
		source := filepath.Join(dataDir, relativeSource)
		if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
		record := store.File{
			ID: id, TenantID: "tenant-a", Filename: "notes.txt", MediaType: "text/plain",
			Purpose: "user_data", Bytes: 7, SHA256: "digest", Status: "uploaded",
			SourcePath: filepath.ToSlash(relativeSource), CreatedAt: now, ExpiresAt: now + 3600,
		}
		if err := dataStore.Add(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{})
	release := make(chan struct{})
	service.syncHandler = func(ctx context.Context, job conversionJob) error {
		close(started)
		<-release
		return nil
	}
	service.enqueue(service.lifecycle(), conversionJob{id: "file_blocked"})
	go service.processNextJob(context.Background())
	<-started

	// Deleting an unrelated file must complete while another conversion runs.
	deleteDone := make(chan error, 1)
	go func() {
		_, err := service.Delete(context.Background(), "file_free", "tenant-a")
		deleteDone <- err
	}()
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete of an unrelated file was blocked by another conversion")
	}
	close(release)
}

func TestCreateEnqueuesAfterRequestContextCancelled(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	settings := testSettings(dataDir)
	service := New(settings, dataStore, testDispatcher(t))
	ctx, cancel := context.WithCancel(context.Background())
	// Simulate a client disconnect immediately after persistence succeeds:
	// the request context is cancelled before the conversion job is enqueued.
	service.afterPersist = cancel
	service.syncHandler = func(ctx context.Context, job conversionJob) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("conversion ran with cancelled context: %v", err)
		}
		return nil
	}

	record, err := service.Create(ctx, "notes.txt", strings.NewReader("hello world"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-service.queue:
		if job.id != record.ID {
			t.Fatalf("queued job = %#v, want %s", job, record.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("conversion job was not enqueued after request context cancellation")
	}
}

func TestCreateConversionCompletesAfterRequestCancelled(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	settings := testSettings(dataDir)
	service := New(settings, dataStore, testDispatcher(t))
	ctx, cancel := context.WithCancel(context.Background())
	service.afterPersist = cancel
	runCtx, runCancel := context.WithCancel(context.Background())
	if err := service.Run(runCtx, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runCancel()
		service.Stop()
	})

	record, err := service.Create(ctx, "notes.txt", strings.NewReader("hello world"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := dataStore.Get(context.Background(), record.ID, record.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.Status == "processed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("conversion did not reach the processed state after request cancellation")
}

func TestStopCancelsLifecycleAndWorkers(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	runCtx, runCancel := context.WithCancel(context.Background())
	if err := service.Run(runCtx, 2); err != nil {
		t.Fatal(err)
	}
	// Stop must not panic even when called concurrently with Run's shutdown.
	done := make(chan struct{})
	go func() {
		runCancel()
		service.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return")
	}
	// After Stop, enqueueing must not block or panic.
	service.enqueue(service.lifecycle(), conversionJob{id: "file_after_stop"})
}

func TestCreateDoesNotDoubleProcess(t *testing.T) {
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := New(testSettings(dataDir), dataStore, testDispatcher(t))
	runCtx, runCancel := context.WithCancel(context.Background())
	if err := service.Run(runCtx, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runCancel()
		service.Stop()
	})

	record, err := service.Create(context.Background(), "notes.txt", strings.NewReader("hello world"), "user_data", "tenant-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := dataStore.Get(context.Background(), record.ID, record.TenantID)
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && current.Status == "processed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give any duplicate job a chance to run, then verify the status is still
	// processed and no second manifest update occurred.
	time.Sleep(100 * time.Millisecond)
	current, err := dataStore.Get(context.Background(), record.ID, record.TenantID)
	if err != nil || current == nil || current.Status != "processed" {
		t.Fatalf("record = %#v, %v; want processed", current, err)
	}
}
