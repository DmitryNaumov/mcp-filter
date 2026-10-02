package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var fingerprintKey = randomBytes(32)
var runID = hex.EncodeToString(randomBytes(16))
var callSequence atomic.Uint64

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("diagnostic randomness unavailable")
	}
	return b
}

// A directory-local key permits comparison across runs without exposing a
// dictionary-verifiable hash of private error text. Never log or export the key.
func loadFingerprintKey(directory string) error {
	path := filepath.Join(directory, ".fingerprint-key")
	// Publish a complete key atomically: several entries may start together.
	f, err := os.CreateTemp(directory, ".fingerprint-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(fingerprintKey)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("diagnostic fingerprint key must be a private regular file")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(key) != 32 {
		return errors.New("invalid diagnostic fingerprint key length")
	}
	fingerprintKey = key
	return nil
}

func messageFingerprint(message string) string {
	h := hmac.New(sha256.New, fingerprintKey)
	h.Write([]byte(message))
	return "hmac-sha256:" + hex.EncodeToString(h.Sum(nil))
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "context_cancelled"
	case errors.Is(err, syscall.EIO):
		return "io_error"
	case errors.Is(err, mcp.ErrConnectionClosed):
		return "connection_closed"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	default:
		return "other"
	}
}

func errorFields(err error) []any {
	return []any{"error_class", errorClass(err), "error_fingerprint", messageFingerprint(err.Error())}
}

// Applies also to SDK and watcher errors; raw messages may embed private text.
func safeLogAttr(_ []string, a slog.Attr) slog.Attr {
	if err, ok := a.Value.Any().(error); ok {
		return slog.Group(a.Key, "error_class", errorClass(err), "error_fingerprint", messageFingerprint(err.Error()))
	}
	if a.Key == "uri" {
		return slog.String("uri_fingerprint", messageFingerprint(a.Value.String()))
	}
	return a
}

func logCallResult(ctx context.Context, entry, tool, trigger, id string, start time.Time, result *mcp.CallToolResult, err error) {
	outcome, level := "success", slog.LevelInfo
	fields := []any{"event", "upstream_call_result", "entry", entry, "tool", tool, "trigger", trigger,
		"duration_ms", float64(time.Since(start).Microseconds()) / 1000, "correlation_id", id, "correlation_scope", "filter_local"}
	fields = append(fields, clientContextFields(ctx)...)
	if err != nil {
		outcome, level = "transport_error", slog.LevelError
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			outcome = "cancelled"
			if errors.Is(ctx.Err(), context.Canceled) {
				level = slog.LevelInfo
			}
		}
		fields = append(fields, errorFields(err)...)
	} else if result != nil && result.IsError {
		outcome, level = "upstream_is_error", slog.LevelError
		// Fingerprint only textual error content, not a serialized response with
		// incidental metadata. Nothing from the content is emitted in clear text.
		h := hmac.New(sha256.New, fingerprintKey)
		for _, c := range result.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				fmt.Fprintf(h, "%d:%s", len(text.Text), text.Text)
			}
		}
		fields = append(fields, "error_class", "tool_error", "error_fingerprint", "hmac-sha256:"+hex.EncodeToString(h.Sum(nil)))
	}
	fields = append(fields, "outcome", outcome)
	slog.Log(ctx, level, "upstream tool call completed", fields...)
}

// Mirrors SDK Run's ownership of a single session, while reporting expected
// shutdown separately. EOF is normalized by the SDK's JSON-RPC Wait, not here.
func runClientSession(ctx context.Context, server *mcp.Server, transport mcp.Transport) error {
	connectionID := runID + ":connection:" + strconv.FormatUint(connectionSequence.Add(1), 10)
	ctx = context.WithValue(ctx, connectionContextKey{}, connectionID)
	slog.InfoContext(ctx, "client connection starting", append([]any{"event", "client_connection_start", "client_connection_id", connectionID, "client_connection_scope", "filter_local"}, launcherContextFields()...)...)
	ss, err := server.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	closed := make(chan error, 1)
	go func() { closed <- ss.Wait() }()
	select {
	case err = <-closed:
	case <-ctx.Done():
		_ = ss.Close()
		err = <-closed
		if err == nil {
			err = ctx.Err()
		}
	}
	reason, level := "client_closed", slog.LevelInfo
	if err == nil && errors.Is(ctx.Err(), context.Canceled) {
		reason = "host_cancelled"
	}
	if err != nil {
		reason, level = "client_transport_error", slog.LevelError
		if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			reason, level = "host_cancelled", slog.LevelInfo
			err = nil
		}
	}
	fields := []any{"event", "client_session_end", "reason", reason, "client_connection_id", connectionID, "client_connection_scope", "filter_local"}
	if err != nil {
		fields = append(fields, errorFields(err)...)
	}
	slog.Log(ctx, level, "client session ended", fields...)
	return err
}
