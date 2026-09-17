package files

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
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

	service := New(config.Config{DataDir: dataDir}, dataStore)
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
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, ConversionWorkers: 2,
	}
	service := New(settings, dataStore)
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Start(ctx); err != nil {
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
