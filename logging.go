package main

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
)

// configureLogger installs the process logger. A filter process owns one
// upstream entry, so its diagnostics go to a separate append-only file.
func configureLogger(cfg config.Config, configPath, entry string) (func() error, error) {
	logging := cfg.Logging
	if logging == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
		return func() error { return nil }, nil
	}
	if logging.Directory == nil || strings.TrimSpace(*logging.Directory) == "" {
		return nil, fmt.Errorf("logging.directory is required when logging is configured")
	}

	level, err := logLevel(logging.Level)
	if err != nil {
		return nil, err
	}
	format, err := logFormat(logging.Format)
	if err != nil {
		return nil, err
	}
	directory := *logging.Directory
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(filepath.Dir(configPath), directory)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, entryLogFileName(entry)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(file, options)
	} else {
		handler = slog.NewTextHandler(file, options)
	}
	slog.SetDefault(slog.New(handler).With("entry", entry, "pid", os.Getpid(), "source", "filter"))
	return file.Close, nil
}

func logLevel(value *string) (slog.Level, error) {
	if value == nil || *value == "" {
		return slog.LevelInfo, nil
	}
	switch *value {
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return 0, fmt.Errorf("unsupported logging.level %q", *value)
	}
}

func logFormat(value *string) (string, error) {
	if value == nil || *value == "" {
		return "text", nil
	}
	switch *value {
	case "text", "json":
		return *value, nil
	default:
		return "", fmt.Errorf("unsupported logging.format %q", *value)
	}
}

func entryLogFileName(entry string) string {
	var name strings.Builder
	for _, r := range entry {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			name.WriteRune(r)
		} else {
			name.WriteByte('-')
		}
	}
	result := strings.Trim(name.String(), "-")
	if result != entry || result == "" {
		sum := sha256.Sum256([]byte(entry))
		return fmt.Sprintf("%s--%x.log", result, sum[:4])
	}
	return result + ".log"
}
