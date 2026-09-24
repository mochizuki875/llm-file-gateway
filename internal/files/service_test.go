package files

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if _, _, err := service.Resolve(context.Background(), "file_missing", "tenant-a", "input[0].file_id"); err == nil {
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
			_, _, err := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
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

	gotRecord, gotManifest, err := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	if err != nil {
		t.Fatal(err)
	}
	if gotRecord.ID != record.ID || gotManifest.SchemaVersion != 3 || len(gotManifest.Documents) != 1 {
		t.Fatalf("resolved = %#v, %#v", gotRecord, gotManifest)
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
	_, _, resolveErr := service.Resolve(context.Background(), record.ID, "tenant-a", "input[0].file_id")
	var gatewayError *apierror.Error
	if !errors.As(resolveErr, &gatewayError) || gatewayError.Status != 422 || gatewayError.Code != "file_processing_failed" {
		t.Fatalf("error = %v", resolveErr)
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
			if err := dataStore.UpdateStatus(context.Background(), record.ID, "uploaded", "", ""); err != nil {
				t.Fatal(err)
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
