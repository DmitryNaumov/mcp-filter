package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReportedContextShapesAndPrivacy(t *testing.T) {
	for _, shape := range []string{"object", "json", "header", "invalid", "absent"} {
		t.Run(shape, func(t *testing.T) {
			meta := map[string]any{"PRIVATE unknown metadata": "PRIVATE token"}
			object := map[string]any{"threadId": "PRIVATE thread", "turnId": "PRIVATE turn", "callId": "PRIVATE call", "token": "PRIVATE token"}
			encoded, _ := json.Marshal(object)
			header := ""
			switch shape {
			case "object":
				meta["x-codex-turn-metadata"] = object
			case "json":
				meta["x-codex-turn-metadata"] = string(encoded)
			case "header":
				header = string(encoded)
			case "invalid":
				meta["x-codex-turn-metadata"] = "PRIVATE malformed"
			}
			c := readReportedContext(meta, header, "request_meta")
			if shape == "object" || shape == "json" || shape == "header" {
				if len(c.ids) != 3 {
					t.Fatalf("missing ids: %v", c)
				}
			} else if len(c.ids) != 0 {
				t.Fatalf("invented correlation: %v", c)
			}
			if strings.Contains(fmt.Sprint(c.fields()), "PRIVATE") {
				t.Fatal("private metadata leaked")
			}
			if shape == "header" && c.ids["call"].source != "request_meta_header.x-codex-turn-metadata" {
				t.Fatal("header source lost")
			}
		})
	}
	for _, value := range []string{"", strings.Repeat("s", 513), " padded ", "\n"} {
		c := readReportedContext(map[string]any{"openai/session": value}, "", "request_meta")
		if len(c.ids) != 0 {
			t.Fatal("invalid session id accepted")
		}
	}
	c := readReportedContext(map[string]any{"openai/session": "PRIVATE session", "x-codex-turn-metadata": strings.Repeat("x", 8193)}, "", "request_meta")
	if len(c.ids) != 1 || c.metadataStatus != "invalid" {
		t.Fatal("oversize metadata not bounded")
	}
	if clientIDFingerprint("thread", "same") == clientIDFingerprint("call", "same") {
		t.Fatal("ID types not separated")
	}
}

func TestClientCorrelationAcrossSDKConnection(t *testing.T) {
	buf := captureDiagnostics(t)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	tool := &mcp.Tool{Name: "test_tool", InputSchema: map[string]any{"type": "object"}}
	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	upstream.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE response"}}}, nil
	})
	uc, us := mcp.NewInMemoryTransports()
	upstreamSession, err := upstream.Connect(ctx, us, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamSession.Close()
	upstreamClient, err := mcp.NewClient(&mcp.Implementation{Name: "filter", Version: "1"}, nil).Connect(ctx, uc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamClient.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "filter", Version: "1"}, nil)
	server.AddReceivingMiddleware(clientContextMiddleware("entry"))
	state, err := newToolState(server, upstreamClient, "entry", []*mcp.Tool{tool}, config.Entry{Allow: []string{"test_tool"}}, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	state.publishInitial()
	ct, st := mcp.NewInMemoryTransports()
	ended := make(chan error, 1)
	go func() { ended <- runClientSession(ctx, server, st) }()
	initial := mcp.Meta{"openai/session": "PRIVATE initial session", "x-codex-turn-metadata": map[string]any{"threadId": "PRIVATE initial thread", "turnId": "PRIVATE stale turn", "callId": "PRIVATE stale call"}, "PRIVATE unknown": "PRIVATE token"}
	client := mcp.NewClient(&mcp.Implementation{Name: "codex_cli_rs", Version: "0.160.0"}, nil)
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				req.GetParams().(*mcp.InitializeParams).Meta = initial
			}
			return next(ctx, method, req)
		}
	})
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	inputs := []*mcp.CallToolParams{
		{Name: "test_tool", Arguments: map[string]any{"token": "PRIVATE argument"}, Meta: mcp.Meta{"x-codex-turn-metadata": `{"threadId":"PRIVATE call thread","turnId":"PRIVATE current turn","callId":"PRIVATE first call"}`, "unrecognized": "PRIVATE token"}},
		{Name: "call_tool", Arguments: map[string]any{"name": "test_tool", "arguments": map[string]any{}}, Meta: mcp.Meta{"x-codex-turn-metadata": map[string]any{"turnId": "PRIVATE second turn", "callId": "PRIVATE second call"}}},
		{Name: "test_tool", Arguments: map[string]any{}},
	}
	for _, params := range inputs {
		if _, err := session.CallTool(ctx, params); err != nil {
			t.Fatal(err)
		}
	}
	session.Close()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("session did not end")
	}
	starts := diagnosticEvents(t, buf, "client_connection_start")
	initialized := diagnosticEvents(t, buf, "client_initialize_context")
	calls := diagnosticEvents(t, buf, "client_call_context")
	results := diagnosticEvents(t, buf, "upstream_call_result")
	ends := diagnosticEvents(t, buf, "client_session_end")
	if len(starts) != 1 || len(initialized) != 1 || len(calls) != 3 || len(results) != 3 || len(ends) != 1 {
		t.Fatalf("wrong event counts: %s", buf)
	}
	conn := starts[0]["client_connection_id"]
	for _, row := range append(append(append(initialized, calls...), results...), ends...) {
		if row["client_connection_id"] != conn || conn == "" {
			t.Fatalf("connection link lost: %v", row)
		}
	}
	if initialized[0]["initialize_success"] != true || initialized[0]["client_family"] != "codex" || initialized[0]["client_version"] != "0.160.0" {
		t.Fatalf("client identity missing: %v", initialized)
	}
	for i, row := range results {
		if row["client_request_id"] != calls[i]["client_request_id"] || row["tool"] != "test_tool" {
			t.Fatalf("call/result join lost: %v", row)
		}
	}
	if results[0]["client_thread_id_fingerprint"] != clientIDFingerprint("thread", "PRIVATE call thread") || results[0]["client_thread_id_source"] != "request_meta.x-codex-turn-metadata" {
		t.Fatal("per-call context not preferred")
	}
	if results[1]["client_thread_id_fingerprint"] != clientIDFingerprint("thread", "PRIVATE initial thread") || results[1]["client_thread_id_source"] != "initialize_meta.x-codex-turn-metadata" || results[1]["client_tool"] != "call_tool" {
		t.Fatal("helper lost connection context")
	}
	if results[1]["client_call_id_fingerprint"] != clientIDFingerprint("call", "PRIVATE second call") {
		t.Fatal("helper call context lost")
	}
	if results[2]["client_turn_id_fingerprint"] != nil || results[2]["client_call_id_fingerprint"] != nil || results[2]["codex_turn_metadata_present"] != false {
		t.Fatal("stale turn/call carried across requests")
	}
	if results[2]["client_session_id_fingerprint"] != clientIDFingerprint("session", "PRIVATE initial session") {
		t.Fatal("session fallback missing")
	}

	// A new SDK connection must not inherit the first one's context.
	ct2, st2 := mcp.NewInMemoryTransports()
	ended2 := make(chan error, 1)
	go func() { ended2 <- runClientSession(ctx, server, st2) }()
	plain, err := mcp.NewClient(&mcp.Implementation{Name: "other", Version: "PRIVATE version"}, nil).Connect(ctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()

	// Concurrent calls on one connection retain their own turn/call context.
	concurrentDone := make(chan error, 2)
	for _, id := range []string{"parallel-a", "parallel-b"} {
		go func(id string) {
			_, err := plain.CallTool(ctx, &mcp.CallToolParams{Name: "test_tool", Arguments: map[string]any{}, Meta: mcp.Meta{"x-codex-turn-metadata": map[string]any{"turnId": id, "callId": id}}})
			concurrentDone <- err
		}(id)
	}
	for i := 0; i < 2; i++ {
		if err := <-concurrentDone; err != nil {
			t.Fatal(err)
		}
	}
	parallelResults := diagnosticEvents(t, buf, "upstream_call_result")
	if len(parallelResults) != 5 {
		t.Fatal("missing concurrent results")
	}
	seen := map[string]bool{}
	for _, row := range parallelResults[3:] {
		matched := ""
		for _, id := range []string{"parallel-a", "parallel-b"} {
			if row["client_call_id_fingerprint"] == clientIDFingerprint("call", id) {
				matched = id
				if row["client_turn_id_fingerprint"] != clientIDFingerprint("turn", id) {
					t.Fatal("turn/call context crossed")
				}
			}
		}
		if matched == "" || seen[matched] {
			t.Fatal("parallel call context lost")
		}
		seen[matched] = true
	}
	if _, err := plain.CallTool(ctx, &mcp.CallToolParams{Name: "test_tool", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	plain.Close()
	select {
	case err := <-ended2:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("second session did not end")
	}
	allResults := diagnosticEvents(t, buf, "upstream_call_result")
	if len(allResults) != 6 {
		t.Fatal("missing second connection result")
	}
	last := allResults[5]
	if last["client_connection_id"] == conn || last["client_connection_id"] == "" || last["client_thread_id_fingerprint"] != nil || last["client_session_id_fingerprint"] != nil {
		t.Fatalf("context leaked between connections: %v", last)
	}
	if strings.Contains(buf.String(), "PRIVATE") {
		t.Fatalf("private data leaked: %s", buf)
	}
}

func TestLauncherThreadIsNotRequestContext(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "PRIVATE launcher thread")
	fields := fmt.Sprint(launcherContextFields())
	if strings.Contains(fields, "PRIVATE") || !strings.Contains(fields, "launcher_thread_id_fingerprint") {
		t.Fatal("unsafe launcher context")
	}
	if strings.Contains(fields, "client_thread_id_fingerprint") {
		t.Fatal("launcher mistaken for request thread")
	}
}
