package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiscoveryHelpersAdvertiseWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	state := &toolState{server: server, tools: map[string]*mcp.Tool{}, configured: true}
	state.addHelpers()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	search := byName["search_tools"]
	if search == nil || !strings.Contains(search.Description, "hidden") || !strings.Contains(search.Description, `{"keywords":["issue","task"]}`) || !strings.Contains(search.Description, "describe_tool") {
		t.Fatalf("search_tools does not explain discovery: %#v", search)
	}
	schema := search.InputSchema.(map[string]any)
	if !reflect.DeepEqual(schema["required"], []any{"keywords"}) {
		t.Fatalf("keywords is not required: %#v", schema["required"])
	}
	properties := schema["properties"].(map[string]any)
	if len(properties) != 1 {
		t.Fatalf("search should expose only keywords: %#v", properties)
	}
	keywords := properties["keywords"].(map[string]any)
	if keywords["type"] != "array" || keywords["minItems"] != float64(1) || keywords["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("keywords must be a nonempty string array: %#v", keywords)
	}
	describe := byName["describe_tool"]
	if describe == nil || !strings.Contains(describe.Description, "search_tools") || !strings.Contains(describe.Description, "input schema") || !strings.Contains(describe.Description, "activates") {
		t.Fatalf("describe_tool does not explain its role: %#v", describe)
	}
	call := byName["call_tool"]
	if call == nil || !strings.Contains(call.Description, "describe_tool") || !strings.Contains(call.Description, "hidden") || !strings.Contains(call.Description, "arguments") {
		t.Fatalf("call_tool does not explain invocation: %#v", call)
	}
}

func TestAutoDiscoveryActivationAndReload(t *testing.T) {
	var logs bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	tools := []*mcp.Tool{
		{Name: "a", Description: "Always visible", InputSchema: map[string]any{"type": "object"}},
		{Name: "b", Description: "Find an issue", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}}},
		{Name: "c", Description: "Secret tool", InputSchema: map[string]any{"type": "object"}},
	}
	calls := 0
	for _, tool := range tools {
		upstream.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls++
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	}
	uc, us := mcp.NewInMemoryTransports()
	upstreamSession, err := upstream.Connect(ctx, us, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamSession.Close()
	upstreamClient, err := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "1"}, nil).Connect(ctx, uc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamClient.Close()
	proxy := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	entry := config.Entry{Allow: []string{"a"}, Deny: []string{"c"}}
	state, err := newToolState(proxy, upstreamClient, "test", tools, entry, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state.publishInitial()
	changed := make(chan struct{}, 10)
	dc, ds := mcp.NewInMemoryTransports()
	proxySession, err := proxy.Connect(ctx, ds, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proxySession.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} }}).Connect(ctx, dc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list := func() string {
		t.Helper()
		r, e := client.ListTools(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		var names []string
		for _, tool := range r.Tools {
			names = append(names, tool.Name)
		}
		return strings.Join(names, ",")
	}
	initial := list()
	names := map[string]bool{}
	for _, name := range strings.Split(initial, ",") {
		names[name] = true
	}
	if names["b"] || names["c"] || !names["a"] || !names["search_tools"] || !names["describe_tool"] || !names["call_tool"] {
		t.Fatalf("initial list: %s", initial)
	}
	call := func(name string, args any) (*mcp.CallToolResult, error) {
		return client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	result, err := call("search_tools", map[string]any{"keywords": []string{"issue"}})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "\"b\"") || strings.Contains(text, "\"c\"") {
		t.Fatalf("search result %s", text)
	}
	result, err = call("search_tools", map[string]any{"keywords": []string{"missing", "issue", "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "\"b\"") || strings.Contains(text, "\"c\"") {
		t.Fatalf("multi-keyword search result %s", text)
	}
	result, err = call("search_tools", map[string]any{"keywords": []string{"unmatched"}})
	if err != nil || result.Content[0].(*mcp.TextContent).Text != "[]" {
		t.Fatalf("empty search result: %#v, %v", result, err)
	}
	if _, err := call("search_tools", map[string]any{"keywords": []string{}}); err == nil {
		t.Fatal("empty keyword list accepted")
	}
	if _, err := call("search_tools", map[string]any{"query": "issue"}); err == nil {
		t.Fatal("obsolete query argument accepted")
	}
	if list() != initial {
		t.Fatal("search activated tool")
	}
	if _, err := call("call_tool", map[string]any{"name": "b", "arguments": map[string]any{}}); err == nil {
		t.Fatal("invalid arguments accepted")
	}
	if calls != 0 {
		t.Fatal("invalid call reached upstream")
	}
	if _, err := call("describe_tool", map[string]any{"name": "c"}); err == nil {
		t.Fatal("denied describe accepted")
	}
	if _, err := call("call_tool", map[string]any{"name": "c", "arguments": map[string]any{}}); err == nil {
		t.Fatal("denied call accepted")
	}
	if _, err := call("describe_tool", map[string]any{"name": "b"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("missing list change notification")
	}
	if !strings.Contains(","+list()+",", ",b,") {
		t.Fatal("activated tool not listed")
	}
	if _, err := call("call_tool", map[string]any{"name": "b", "arguments": map[string]any{"id": "private-id-123"}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls: %d", calls)
	}
	if err := state.reload(config.Entry{Allow: []string{"a"}, Deny: []string{"b", "c"}}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(","+list()+",", ",b,") {
		t.Fatal("newly denied tool still listed")
	}
	if _, err := call("call_tool", map[string]any{"name": "b", "arguments": map[string]any{"id": "123"}}); err == nil {
		t.Fatal("newly denied call accepted")
	}
	if calls != 1 {
		t.Fatalf("denied call reached upstream: %d", calls)
	}
	if err := state.reload(entry, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(","+list()+",", ",b,") {
		t.Fatal("revoked tool returned without activation")
	}
	if _, err := call("call_tool", map[string]any{"name": "b", "arguments": map[string]any{"id": "456"}}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(","+list()+",", ",b,") {
		t.Fatalf("generic activation failed: calls=%d list=%s", calls, list())
	}
	newState, err := newToolState(mcp.NewServer(&mcp.Implementation{Name: "new", Version: "1"}, nil), upstreamClient, "test", tools, entry, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	newState.publishInitial()
	if newState.active["b"] || newState.published["b"] {
		t.Fatal("activation leaked into new connection")
	}
	if strings.Contains(logs.String(), "private-id-123") {
		t.Fatal("forwarded tool arguments leaked into logs")
	}
	called := map[string]bool{}
	results := map[int]bool{}
	searchKeywordsLogged := false
	describedToolLogged := false
	calledToolLogged := false
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] == "helper tool called" {
			toolName := event["tool"].(string)
			called[toolName] = true
			if event["entry"] != "test" {
				t.Fatalf("helper call missing entry: %#v", event)
			}
			if toolName == "search_tools" && event["keywords"] == nil {
				searchKeywordsLogged = true
			}
			if toolName == "describe_tool" && event["target"] == "b" {
				describedToolLogged = true
			}
			if toolName == "call_tool" && event["target"] == "b" {
				calledToolLogged = true
			}
		}
		if event["msg"] == "search_tools result" {
			results[int(event["match_count"].(float64))] = true
			if event["result"] != nil {
				t.Fatal("search result leaked")
			}
		}
	}
	for _, name := range helperNames {
		if !called[name] {
			t.Fatalf("missing %s call log: %s", name, logs.String())
		}
	}
	if !results[1] || !results[0] {
		t.Fatalf("missing search result logs: %s", logs.String())
	}
	if !searchKeywordsLogged || !describedToolLogged || !calledToolLogged {
		t.Fatalf("helper call logs leak keywords or lack target names: %s", logs.String())
	}
}

func TestAutoRejectsHelperCollision(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	_, err := newToolState(server, nil, "test", []*mcp.Tool{{Name: "call_tool", InputSchema: map[string]any{"type": "object"}}}, config.Entry{}, true, 0)
	if err == nil {
		t.Fatal("helper collision accepted")
	}
}

func TestUpstreamToolChangesRefreshPublishedCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream := mcp.NewServer(&mcp.Implementation{Name: "dynamic", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	tool := func(name, description string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object"}}
	}
	handler := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	upstream.AddTool(tool("a", "old"), handler)
	uc, us := mcp.NewInMemoryTransports()
	upstreamSession, err := upstream.Connect(ctx, us, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamSession.Close()
	changes := make(chan struct{}, 1)
	upstreamClient, err := newUpstreamClient(func(kind string) {
		if kind == "tools" {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}).Connect(ctx, uc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamClient.Close()
	proxy := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}})
	entry := config.Entry{Allow: []string{"a", "b"}, Deny: []string{"secret"}, Metadata: config.Metadata{Tools: map[string]map[string]any{"b": {"description": "patched"}}}}
	state, err := newToolState(proxy, upstreamClient, "test", []*mcp.Tool{tool("a", "old")}, entry, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state.publishInitial()
	go watchUpstreamTools(ctx, changes, state)
	dc, ds := mcp.NewInMemoryTransports()
	proxySession, err := proxy.Connect(ctx, ds, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proxySession.Close()
	downstreamChanges := make(chan struct{}, 10)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "downstream", Version: "1"}, &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
		downstreamChanges <- struct{}{}
	}}).Connect(ctx, dc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list := func() map[string]*mcp.Tool {
		t.Helper()
		result, err := client.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := make(map[string]*mcp.Tool)
		for _, item := range result.Tools {
			tools[item.Name] = item
		}
		return tools
	}
	waitFor := func(check func(map[string]*mcp.Tool) bool) {
		t.Helper()
		for ctx.Err() == nil {
			if check(list()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for refreshed tool list")
	}
	upstream.AddTool(tool("b", "upstream"), handler)
	upstream.AddTool(tool("secret", "hidden"), handler)
	waitFor(func(tools map[string]*mcp.Tool) bool { return tools["b"] != nil && tools["b"].Description == "patched" })
	select {
	case <-downstreamChanges:
	case <-ctx.Done():
		t.Fatal("missing downstream list change after successful refresh")
	}
	if list()["secret"] != nil {
		t.Fatal("denied upstream tool was published")
	}
	upstream.RemoveTools("a", "b")
	waitFor(func(tools map[string]*mcp.Tool) bool { return tools["a"] == nil && tools["b"] == nil })
	if _, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"name": "b", "arguments": map[string]any{}}}); err == nil {
		t.Fatal("removed upstream tool remained callable")
	}
}

func TestUpstreamRefreshFailurePreservesPublishedTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream := mcp.NewServer(&mcp.Implementation{Name: "dynamic", Version: "1"}, nil)
	tool := &mcp.Tool{Name: "a", InputSchema: map[string]any{"type": "object"}}
	upstream.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
	uc, us := mcp.NewInMemoryTransports()
	upstreamSession, err := upstream.Connect(ctx, us, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamSession.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "1"}, nil).Connect(ctx, uc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	proxy := mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "1"}, nil)
	entry := config.Entry{Allow: []string{"a", "b"}, Metadata: config.Metadata{Tools: map[string]map[string]any{"b": {"name": "renamed"}}}}
	state, err := newToolState(proxy, client, "test", []*mcp.Tool{tool}, entry, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state.publishInitial()
	upstream.AddTool(&mcp.Tool{Name: "b", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
	if err := state.refreshUpstream(ctx); err == nil {
		t.Fatal("invalid metadata patch was accepted")
	}
	if !state.published["a"] || state.published["b"] || state.tools["b"] != nil {
		t.Fatal("failed refresh changed the published catalog")
	}
	entry.Metadata.Tools["b"] = map[string]any{"inputSchema": map[string]any{"type": "string"}}
	if err := state.reload(entry, true); err != nil {
		t.Fatal(err)
	}
	if err := state.refreshUpstream(ctx); err == nil {
		t.Fatal("invalid tool schema was accepted")
	}
	if !state.published["a"] || state.published["b"] || state.tools["b"] != nil {
		t.Fatal("invalid tool schema changed the published catalog")
	}
}
