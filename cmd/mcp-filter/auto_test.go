package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcecraft/mcp-filter/internal/config"
)

func TestAutoDiscoveryActivationAndReload(t *testing.T) {
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
	result, err := call("search_tools", map[string]any{"query": "issue"})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "\"b\"") || strings.Contains(text, "\"c\"") {
		t.Fatalf("search result %s", text)
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
	if _, err := call("call_tool", map[string]any{"name": "b", "arguments": map[string]any{"id": "123"}}); err != nil {
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
}

func TestAutoRejectsHelperCollision(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	_, err := newToolState(server, nil, "test", []*mcp.Tool{{Name: "call_tool", InputSchema: map[string]any{"type": "object"}}}, config.Entry{}, true, 0)
	if err == nil {
		t.Fatal("helper collision accepted")
	}
}
