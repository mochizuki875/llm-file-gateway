package logging

import (
	"context"
	"log/slog"
)

// levelStep is the number of slog levels between verbosity steps.
const levelStep = 4

// Level maps a verbosity value (0-2) to an slog level: 0 is info, 1 is debug,
// and 2 is a more verbose debug level.
func Level(verbosity int) slog.Level {
	return slog.LevelInfo - slog.Level(verbosity*levelStep)
}

// ReplaceLevel clamps levels below debug to debug so that verbose levels are
// still rendered as DEBUG in the text output.
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

// V logs a message at the level corresponding to the given verbosity.
func V(ctx context.Context, verbosity int, message string, attributes ...any) {
	slog.Log(ctx, Level(verbosity), message, attributes...)
}
