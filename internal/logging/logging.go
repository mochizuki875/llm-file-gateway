package logging

import (
	"context"
	"log/slog"
)

const levelStep = 4

func Level(verbosity int) slog.Level {
	return slog.LevelInfo - slog.Level(verbosity*levelStep)
}

func ReplaceLevel(_ []string, attribute slog.Attr) slog.Attr {
	if attribute.Key != slog.LevelKey {
		return attribute
	}
	level, ok := attribute.Value.Any().(slog.Level)
	if ok && level < slog.LevelDebug {
		return slog.String(slog.LevelKey, slog.LevelDebug.String())
	}
	return attribute
}

func V(ctx context.Context, verbosity int, message string, attributes ...any) {
	slog.Log(ctx, Level(verbosity), message, attributes...)
}
