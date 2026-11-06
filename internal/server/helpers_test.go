package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

func testDependencies(t *testing.T) (config.Config, *store.Store, *files.Service) {
	t.Helper()
	dataDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(dataDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	settings := config.Config{
		DataDir: dataDir, FileTTL: 5 * time.Minute, MaxFileBytes: 1024,
		MaxDocumentPages: 20, MaxDocumentTextChars: 500_000, Workers: 2,
		VLLMAPIKey: "upstream-key",
	}
	if err := os.Mkdir(filepath.Join(dataDir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := files.New(settings, dataStore, converter.NewDispatcher(converter.NewInTreeRegistry(), converter.DefaultConverterConfig(), nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Run(ctx, settings.Workers); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.Stop()
		_ = dataStore.Close()
	})
	return settings, dataStore, service
}
