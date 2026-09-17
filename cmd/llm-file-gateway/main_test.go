package main

import (
	"bytes"
	"context"
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
	logger.Log(context.Background(), logging.Level(2), "trace message", "verbosity", 2)

	if !strings.Contains(output.String(), "level=DEBUG") || strings.Contains(output.String(), "level=DEBUG-4") {
		t.Fatalf("log output = %q, want level=DEBUG without slog level offset", output.String())
	}
}
