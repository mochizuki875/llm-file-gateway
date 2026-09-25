package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
	"github.com/mochizuki875/llm-file-gateway/internal/logging"
	"github.com/mochizuki875/llm-file-gateway/internal/server"
	"github.com/mochizuki875/llm-file-gateway/internal/store"
)

// main is the entry point of the gateway process.
func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

// run loads the configuration, starts the file service and the HTTP server,
// and blocks until the server is shut down or a fatal error occurs.
func run() error {
	settings, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(newLogger(os.Stderr, settings.LogVerbosity))
	workDir := filepath.Join(settings.DataDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	if err := cleanupStaleRequestDirectories(workDir); err != nil {
		return err
	}
	dataStore, err := store.OpenDataDir(settings.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := dataStore.Close(); err != nil {
			slog.Warn("database close failed", "error", err)
		}
	}()
	if !settings.GatewayAuthRequired {
		if err := dataStore.ConsolidateTenants(context.Background()); err != nil {
			return err
		}
	}

	// Set up context for graceful shutdown on SIGINT or SIGTERM signals.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Set up the default converter registry.
	registry := converter.NewInTreeRegistry()

	// Set up the file service with the converter dispatcher.
	converterConfig := converter.DefaultConverterConfig()
	converterConfig.DPI = settings.DocumentDPI
	fileService := files.New(settings, dataStore, converter.NewDispatcher(registry, converterConfig, converter.SettingsHandle{SettingsValue: settings}))
	if err := fileService.ReconcileStorage(context.Background()); err != nil {
		return err
	}

	// Start the file service.
	// Worker goroutines will start and begin processing pending files.
	if err := fileService.Run(ctx, settings.Workers); err != nil {
		return err
	}
	defer fileService.Stop()

	// Set up the HTTP server for the gateway.
	httpServer := &http.Server{
		Addr:              settings.Address,
		Handler:           server.NewHandler(settings, dataStore, fileService),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errorsChannel := make(chan error, 1)
	go func() {
		slog.Info("starting gateway", "address", settings.Address, "model", settings.VLLMModel, "verbosity", settings.LogVerbosity)
		errorsChannel <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errorsChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// cleanupStaleRequestDirectories removes request-scoped directories left by a
// previous process without touching unrelated work files or symlinks.
func cleanupStaleRequestDirectories(workDir string) error {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("read work directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "gateway-request-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(workDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale request directory %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// newLogger builds a text logger whose level is derived from the configured
// verbosity (0: info, 1: debug, 2: verbose debug).
func newLogger(output io.Writer, verbosity int) *slog.Logger {
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{
		Level:       logging.Level(verbosity),
		ReplaceAttr: logging.ReplaceLevel,
	}))
}
