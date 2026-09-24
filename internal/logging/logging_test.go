package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLevelMapsVerbosity(t *testing.T) {
	for _, test := range []struct {
		verbosity int
		want      slog.Level
	}{
		{verbosity: 0, want: slog.LevelInfo},
		{verbosity: 1, want: slog.LevelDebug},
		{verbosity: 2, want: slog.LevelDebug - 4},
	} {
		if got := Level(test.verbosity); got != test.want {
			t.Fatalf("Level(%d) = %v, want %v", test.verbosity, got, test.want)
		}
	}
}

func TestReplaceLevelNormalizesVerboseLevels(t *testing.T) {
	attribute := slog.Attr{Key: slog.LevelKey, Value: slog.AnyValue(slog.LevelDebug - 4)}
	replaced := ReplaceLevel(nil, attribute)
	if replaced.Value.String() != slog.LevelDebug.String() {
		t.Fatalf("replaced level = %q, want %q", replaced.Value.String(), slog.LevelDebug.String())
	}

	info := slog.Attr{Key: slog.LevelKey, Value: slog.AnyValue(slog.LevelInfo)}
	if replaced := ReplaceLevel(nil, info); replaced.Value.String() != slog.LevelInfo.String() {
		t.Fatalf("info level = %q, want %q", replaced.Value.String(), slog.LevelInfo.String())
	}

	other := slog.String("message", "hello")
	if replaced := ReplaceLevel(nil, other); replaced.Key != other.Key || replaced.Value.String() != other.Value.String() {
		t.Fatalf("non-level attribute was modified: %#v", replaced)
	}
}

func TestVLogsAtVerboseLevel(t *testing.T) {
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug - 4})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	V(context.Background(), 2, "verbose message", "file_id", "file_1")
	if !strings.Contains(output.String(), "verbose message") || !strings.Contains(output.String(), "file_id=file_1") {
		t.Fatalf("log output = %q", output.String())
	}
}
