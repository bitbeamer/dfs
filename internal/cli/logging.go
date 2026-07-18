package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

func newMountLogger(levelName, logFile string, stderr io.Writer, forceDebug bool) (*slog.Logger, io.Closer, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(levelName)); err != nil {
		return nil, nil, fmt.Errorf("invalid log level %q (use debug, info, warn, or error)", levelName)
	}
	if forceDebug {
		level = slog.LevelDebug
	}

	writer := stderr
	var closer io.Closer
	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", logFile, err)
		}
		closer = file
		writer = io.MultiWriter(stderr, file)
	}

	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level}))
	return logger, closer, nil
}
