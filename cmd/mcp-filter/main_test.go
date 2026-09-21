package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOverlayToolPreservesNameAndMergesMetadata(t *testing.T) {
	tool := &mcp.Tool{
		Name:        "get_issue",
		Description: "original",
		InputSchema: map[string]any{"type": "object"},
	}
	overlaid, err := overlayTool(tool, map[string]any{
		"description": "project-specific",
		"annotations": map[string]any{"readOnlyHint": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if overlaid.Name != "get_issue" || overlaid.Description != "project-specific" {
		t.Fatalf("unexpected overlaid tool: %#v", overlaid)
	}
	if overlaid.Annotations == nil || !overlaid.Annotations.ReadOnlyHint {
		t.Fatalf("annotation was not applied: %#v", overlaid.Annotations)
	}
}

func TestHeaderTransportReadsValueFromEnvironment(t *testing.T) {
	t.Setenv("MCP_FILTER_TEST_TOKEN", "secret-value")
	transport := headerTransport{
		headerEnv: headerEnvFlags{"Authorization=MCP_FILTER_TEST_TOKEN"},
		next: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get("Authorization"); got != "secret-value" {
				t.Fatalf("unexpected authorization header: %q", got)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOverlayToolRejectsToolRename(t *testing.T) {
	tool := &mcp.Tool{Name: "get_issue", InputSchema: map[string]any{"type": "object"}}
	if _, err := overlayTool(tool, map[string]any{"name": "other"}); err == nil {
		t.Fatal("renaming a tool should fail")
	}
}

func TestOverlayServerChangesDisplayMetadataButNotIdentity(t *testing.T) {
	server, instructions, err := overlayServer("tracker", nil, map[string]any{
		"title":        "Project tracker",
		"instructions": "Use issue keys from this repository.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.Name != "tracker" || server.Title != "Project tracker" {
		t.Fatalf("unexpected server metadata: %#v", server)
	}
	if instructions != "Use issue keys from this repository." {
		t.Fatalf("unexpected instructions: %q", instructions)
	}
}

func TestOverlayServerRejectsRename(t *testing.T) {
	if _, _, err := overlayServer("tracker", nil, map[string]any{"name": "other"}); err == nil {
		t.Fatal("renaming a server should fail")
	}
}

func TestParseProxyFlags(t *testing.T) {
	opts, command, err := parseProxyFlags([]string{"--entry", "tracker", "--transport", "stdio", "--", "fake-mcp", "--serve"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.entry != "tracker" || len(command) != 2 || command[0] != "fake-mcp" {
		t.Fatalf("unexpected result: %#v %#v", opts, command)
	}
}

func TestConnectStreamableHTTPUpstream(t *testing.T) {
	upstream := testUpstream()
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil))
	defer httpServer.Close()

	tools := connectRemoteForTest(t, "streamable-http", httpServer.URL)
	if len(tools) != 1 || tools[0].Name != "visible" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
}

func TestConnectLegacySSEUpstream(t *testing.T) {
	upstream := testUpstream()
	httpServer := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return upstream }, nil))
	defer httpServer.Close()

	tools := connectRemoteForTest(t, "sse", httpServer.URL)
	if len(tools) != 1 || tools[0].Name != "visible" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
}

func connectRemoteForTest(t *testing.T, transport, endpoint string) []*mcp.Tool {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), ".mcp-filter.json")
	contents := []byte(`{"entries":{"test":{"allow":["visible"]}}}`)
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, session, tools, err := connect(ctx, proxyOptions{entry: "test", config: configPath, transport: transport, url: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	return tools
}

func testUpstream() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-upstream", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "visible", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	return server
}

func TestArgumentsJSONRoundTrip(t *testing.T) {
	arguments := map[string]any{"id": "ABC-1", "nested": map[string]any{"value": true}}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.(map[string]any)["id"] != "ABC-1" {
		t.Fatalf("unexpected argument round trip: %#v", decoded)
	}
}
