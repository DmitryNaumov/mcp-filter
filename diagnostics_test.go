package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: safeLogAttr})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func diagnosticEvents(t *testing.T, buf *bytes.Buffer, event string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] == event {
			events = append(events, row)
		}
	}
	return events
}

func TestForwardDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, outcome, level string }{
		{"success", "success", "INFO"}, {"is_error", "upstream_is_error", "ERROR"},
		{"transport", "transport_error", "ERROR"}, {"timeout", "timeout", "ERROR"},
		{"cancel", "cancelled", "INFO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDiagnostics(t)
			ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			tool := &mcp.Tool{Name: "test_tool", InputSchema: map[string]any{"type": "object"}}
			entered := make(chan struct{}, 2)
			upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
			upstream.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				entered <- struct{}{}
				if tc.name == "timeout" || tc.name == "cancel" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &mcp.CallToolResult{IsError: tc.name == "is_error", Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE response secret"}}}, nil
			})
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			ss, err := upstream.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "filter", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			timeout := time.Duration(0)
			if tc.name == "timeout" {
				timeout = 30 * time.Millisecond
			}
			state, err := newToolState(mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "1"}, nil), client, "entry", []*mcp.Tool{tool}, config.Entry{}, false, timeout)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "transport" {
				ss.Close()
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.name == "cancel" {
				go func() { <-entered; cancel() }()
			}
			for _, trigger := range []string{"direct_call", "call_tool"} {
				if tc.name == "cancel" && trigger == "call_tool" {
					break
				}
				if trigger == "call_tool" {
					_, err = state.call(callCtx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"name":"test_tool","arguments":{"secret":"PRIVATE argument secret"}}`)}})
				} else {
					_, err = state.forward(callCtx, "test_tool", map[string]any{"secret": "PRIVATE argument secret"}, false, trigger)
				}
				if tc.name == "success" || tc.name == "is_error" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("expected failure")
				}
			}
			events := diagnosticEvents(t, buf, "upstream_call_result")
			expected := 2
			if tc.name == "cancel" {
				expected = 1
			}
			if len(events) != expected {
				t.Fatalf("events=%v logs=%s", events, buf)
			}
			ids := map[string]bool{}
			for _, row := range events {
				if row["outcome"] != tc.outcome || row["level"] != tc.level || row["entry"] != "entry" || row["tool"] != "test_tool" || row["correlation_scope"] != "filter_local" {
					t.Fatalf("bad event: %v", row)
				}
				id, ok := row["correlation_id"].(string)
				if !ok || id == "" || ids[id] {
					t.Fatalf("invalid correlation: %v", row)
				}
				ids[id] = true
				if row["duration_ms"].(float64) < 0 {
					t.Fatalf("invalid duration: %v", row)
				}
				if tc.name != "success" && (row["error_class"] == nil || row["error_fingerprint"] == nil) {
					t.Fatalf("missing safe error: %v", row)
				}
			}
			if strings.Contains(buf.String(), "PRIVATE") || strings.Contains(buf.String(), `"arguments"`) || strings.Contains(buf.String(), `"result"`) {
				t.Fatalf("private payload leaked: %s", buf)
			}
		})
	}
}

func TestClientSessionEndDiagnostics(t *testing.T) {
	for _, cancelHost := range []bool{false, true} {
		t.Run(map[bool]string{false: "client_close", true: "host_cancel"}[cancelHost], func(t *testing.T) {
			buf := captureDiagnostics(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "1"}, nil)
			ct, st := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() { done <- runClientSession(ctx, server, st) }()
			connectCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			client, err := mcp.NewClient(&mcp.Implementation{Name: "host", Version: "1"}, nil).Connect(connectCtx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cancelHost {
				cancel()
			} else {
				client.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("session did not end")
			}
			client.Close()
			rows := diagnosticEvents(t, buf, "client_session_end")
			if len(rows) != 1 || rows[0]["level"] != "INFO" {
				t.Fatalf("unexpected end: %s", buf)
			}
			reason := "client_closed"
			if cancelHost {
				reason = "host_cancelled"
			}
			if rows[0]["reason"] != reason {
				t.Fatalf("unexpected reason: %s", buf)
			}
		})
	}
}

func TestErrorFingerprintPrivacyAndStability(t *testing.T) {
	buf := captureDiagnostics(t)
	secret := errors.New("PRIVATE token=secret argument response")
	slog.Error("failure", "error", secret)
	if strings.Contains(buf.String(), "PRIVATE") || strings.Contains(buf.String(), "token=") {
		t.Fatal("raw error leaked")
	}
	if messageFingerprint(secret.Error()) != messageFingerprint(secret.Error()) || messageFingerprint("different") == messageFingerprint(secret.Error()) {
		t.Fatal("unstable or constant fingerprint")
	}
	dir := t.TempDir()
	if err := loadFingerprintKey(dir); err != nil {
		t.Fatal(err)
	}
	before := messageFingerprint("same message")
	fingerprintKey = randomBytes(32)
	if err := loadFingerprintKey(dir); err != nil {
		t.Fatal(err)
	}
	if before != messageFingerprint("same message") {
		t.Fatal("key did not persist")
	}
	// Cancellation returned by a transport without a cancelled parent remains ERROR.
	logCallResult(context.Background(), "entry", "tool", "direct_call", "id", time.Now(), nil, context.Canceled)
	rows := diagnosticEvents(t, buf, "upstream_call_result")
	if len(rows) != 1 || rows[0]["level"] != "ERROR" {
		t.Fatal("unexpected cancellation downgraded")
	}
	if errorClass(io.EOF) != "eof" {
		t.Fatal("EOF not classified")
	}
}

// Inject a read failure at the SDK transport boundary, so Wait's real error
// propagation is covered even if the host cancellation races with the failure.
type failedReadTransport struct{ err error }

func (t failedReadTransport) Connect(context.Context) (mcp.Connection, error) {
	return failedReadConnection{err: t.err}, nil
}

type failedReadConnection struct{ err error }

func (c failedReadConnection) Read(context.Context) (jsonrpc.Message, error) { return nil, c.err }
func (failedReadConnection) Write(context.Context, jsonrpc.Message) error    { return nil }
func (failedReadConnection) Close() error                                    { return nil }
func (failedReadConnection) SessionID() string                               { return "" }

func TestClientSessionUnexpectedFailures(t *testing.T) {
	for _, hostCancelled := range []bool{false, true} {
		buf := captureDiagnostics(t)
		ctx, cancel := context.WithCancel(context.Background())
		if hostCancelled {
			cancel()
		}
		failure := errors.New("PRIVATE transport failure token=secret")
		server := mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "1"}, nil)
		err := runClientSession(ctx, server, failedReadTransport{err: failure})
		cancel()
		if !errors.Is(err, failure) {
			t.Fatalf("transport failure lost: %v", err)
		}
		rows := diagnosticEvents(t, buf, "client_session_end")
		if len(rows) != 1 || rows[0]["level"] != "ERROR" || rows[0]["reason"] != "client_transport_error" {
			t.Fatalf("unexpected failure classification: %s", buf)
		}
		if strings.Contains(buf.String(), "PRIVATE") {
			t.Fatal("private transport error leaked")
		}
	}
	buf := captureDiagnostics(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "1"}, nil)
	if err := runClientSession(context.Background(), server, failedReadTransport{err: context.Canceled}); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation lost: %v", err)
	}
	rows := diagnosticEvents(t, buf, "client_session_end")
	if len(rows) != 1 || rows[0]["level"] != "ERROR" {
		t.Fatalf("unexpected cancellation downgraded: %s", buf)
	}
}
