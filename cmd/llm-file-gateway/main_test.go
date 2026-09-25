package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mochizuki875/llm-file-gateway/internal/logging"
)

func TestNewLoggerFiltersByVerbosity(t *testing.T) {
	for _, test := range []struct {
		name      string
		verbosity int
		present   []string
		absent    []string
	}{
		{name: "normal", verbosity: 0, present: []string{"info message", "warn message", "error message"}, absent: []string{"debug message", "trace message"}},
		{name: "debug", verbosity: 1, present: []string{"debug message", "info message", "warn message", "error message"}, absent: []string{"trace message"}},
		{name: "trace", verbosity: 2, present: []string{"trace message", "debug message", "info message", "warn message", "error message"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := newLogger(&output, test.verbosity)
			logger.Log(context.Background(), logging.Level(2), "trace message")
			logger.Debug("debug message")
			logger.Info("info message")
			logger.Warn("warn message")
			logger.Error("error message")

			for _, value := range test.present {
				if !strings.Contains(output.String(), value) {
					t.Errorf("output does not contain %q: %s", value, output.String())
				}
			}
			for _, value := range test.absent {
				if strings.Contains(output.String(), value) {
					t.Errorf("output contains %q: %s", value, output.String())
				}
			}
		})
	}
}

func TestNewLoggerNormalizesVerboseLevelName(t *testing.T) {
	var output bytes.Buffer
	logger := newLogger(&output, 2)
	logger.Log(context.Background(), logging.Level(2), "trace message")

	if !strings.Contains(output.String(), "level=DEBUG") || strings.Contains(output.String(), "level=DEBUG-4") {
		t.Fatalf("log output = %q, want level=DEBUG without slog level offset", output.String())
	}
}

func TestRunRequiresVLLMModel(t *testing.T) {
	clearGatewayEnv(t)
	if err := run(); err == nil || !strings.Contains(err.Error(), "VLLM_MODEL") {
		t.Fatalf("run() error = %v, want VLLM_MODEL required", err)
	}
}

func TestRunRequiresVLLMAPIKey(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:9999/v1")
	if err := run(); err == nil || !strings.Contains(err.Error(), "VLLM_API_KEY") {
		t.Fatalf("run() error = %v, want VLLM_API_KEY required", err)
	}
}

func TestRunRejectsInvalidBaseURL(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "not-a-url")
	t.Setenv("VLLM_API_KEY", "upstream-key")
	if err := run(); err == nil || !strings.Contains(err.Error(), "VLLM_BASE_URL") {
		t.Fatalf("run() error = %v, want VLLM_BASE_URL error", err)
	}
}

func TestRunRejectsInvalidPort(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:9999/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")
	t.Setenv("GATEWAY_PORT", "70000")
	if err := run(); err == nil || !strings.Contains(err.Error(), "GATEWAY_PORT") {
		t.Fatalf("run() error = %v, want GATEWAY_PORT error", err)
	}
}

func TestRunRejectsInvalidDataDir(t *testing.T) {
	clearGatewayEnv(t)
	t.Setenv("VLLM_MODEL", "test-model")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:9999/v1")
	t.Setenv("VLLM_API_KEY", "upstream-key")
	// A path whose parent is a file cannot be created as a directory.
	parent := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_DATA_DIR", filepath.Join(parent, "gateway-data"))
	if err := run(); err == nil {
		t.Fatal("run() succeeded with an invalid data directory")
	}
}

func TestCleanupStaleRequestDirectories(t *testing.T) {
	workDir := t.TempDir()
	stale := filepath.Join(workDir, "gateway-request-old")
	unrelated := filepath.Join(workDir, "do-not-delete")
	target := filepath.Join(workDir, "symlink-target")
	for _, directory := range []string{stale, unrelated, target} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(workDir, "gateway-request-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "gateway-request-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleRequestDirectories(workDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory still exists: %v", err)
	}
	for _, path := range []string{unrelated, target, link, filepath.Join(workDir, "gateway-request-file")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unrelated path %q was removed: %v", path, err)
		}
	}
}

func TestComposePropagatesMaxRequestBodyBytesWithoutFixedDefault(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `MAX_REQUEST_BODY_BYTES: "${MAX_REQUEST_BODY_BYTES:-}"`) {
		t.Fatalf("compose.yaml does not preserve MAX_REQUEST_BODY_BYTES empty/default semantics")
	}
}

func clearGatewayEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"VLLM_MODEL", "VLLM_BASE_URL", "VLLM_API_KEY", "GATEWAY_API_KEY",
		"GATEWAY_AUTH_REQUIRED", "GATEWAY_HOST", "GATEWAY_PORT",
		"FILE_TTL_SECONDS", "MAX_FILE_BYTES", "MAX_REQUEST_BODY_BYTES", "MAX_DOCUMENT_PAGES",
		"MAX_DOCUMENT_TEXT_CHARS",
		"DOCUMENT_TEXT_EXTRACTION_ENABLED", "CONVERSION_WORKERS",
		"REQUEST_TIMEOUT_SECONDS", "LOGLEVEL", "GATEWAY_DATA_DIR",
	} {
		t.Setenv(name, "")
	}
}
