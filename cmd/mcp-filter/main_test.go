package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcecraft/mcp-filter/internal/config"
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

func TestToolMetadataPatchMergesNamedAndGenericPatches(t *testing.T) {
	entry := config.Entry{Metadata: config.Metadata{
		Tools: map[string]map[string]any{"get_issue": {"description": "named override"}},
		Patches: []config.Patch{{
			Method: "tools/list",
			Select: map[string]any{"name": "get_issue"},
			Patch:  map[string]any{"title": "Read issue"},
		}},
	}}
	patch, err := toolMetadataPatch(entry, &mcp.Tool{Name: "get_issue", InputSchema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	if patch["description"] != "named override" || patch["title"] != "Read issue" {
		t.Fatalf("unexpected metadata patch: %#v", patch)
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

func TestParseProxyFlagsUsesConfiguredToolTimeout(t *testing.T) {
	opts, _, err := parseProxyFlags([]string{"--entry", "tracker", "--timeout", "3s", "--", "fake-mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.timeout != 3*time.Second {
		t.Fatalf("unexpected timeout: %s", opts.timeout)
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

func TestProxyEndToEndFiltersAndForwardsStdioTools(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".mcp-filter.json")
	if err := os.WriteFile(configPath, []byte(`{"entries":{"test":{"allow":["visible"],"metadata":{"patches":[{"method":"tools/list","select":{"name":"visible"},"patch":{"title":"Visible issue"}},{"method":"prompts/list","select":{"name":"status"},"patch":{"title":"Project status"}},{"method":"resources/list","select":{"uri":"test://item"},"patch":{"title":"Project resource"}}]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	proxyCommand := exec.Command(
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--",
		"proxy", "--entry", "test", "--config", configPath, "--transport", "stdio", "--",
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--", "upstream",
	)
	proxyCommand.Env = append(os.Environ(), "MCP_FILTER_TEST_HELPER=1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: proxyCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "visible" {
		t.Fatalf("unexpected published tools: %#v", list.Tools)
	}
	if list.Tools[0].Title != "Visible issue" {
		t.Fatalf("generic metadata patch was not applied: %#v", list.Tools[0])
	}
	prompts, err := session.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Title != "Project status" {
		t.Fatalf("unexpected proxied prompts: %#v", prompts.Prompts)
	}
	resources, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].Title != "Project resource" {
		t.Fatalf("unexpected proxied resources: %#v", resources.Resources)
	}
	resourceResult, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "test://item"})
	if err != nil || len(resourceResult.Contents) != 1 || resourceResult.Contents[0].Text != "resource content" {
		t.Fatalf("unexpected proxied resource: %#v, %v", resourceResult, err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "visible", Arguments: map[string]any{"issue": "ABC-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected result content: %#v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "forwarded:ABC-1" {
		t.Fatalf("unexpected forwarded result: %#v", result.Content)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hidden", Arguments: map[string]any{}}); err == nil {
		t.Fatal("hidden tool call unexpectedly succeeded")
	}
}

func TestProxyTimesOutUpstreamToolCall(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".mcp-filter.json")
	if err := os.WriteFile(configPath, []byte(`{"entries":{"test":{"allow":["slow"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyCommand := exec.Command(
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--",
		"proxy", "--entry", "test", "--config", configPath, "--transport", "stdio", "--timeout", "10ms", "--",
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--", "upstream",
	)
	proxyCommand.Env = append(os.Environ(), "MCP_FILTER_TEST_HELPER=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "timeout-client", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: proxyCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}}); err == nil {
		t.Fatal("slow tool call unexpectedly succeeded")
	}
}

func TestValidateUpstreamRejectsMissingAllowedTool(t *testing.T) {
	t.Setenv("MCP_FILTER_TEST_HELPER", "1")
	configPath := filepath.Join(t.TempDir(), ".mcp-filter.json")
	if err := os.WriteFile(configPath, []byte(`{"entries":{"test":{"allow":["visible","missing"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validate(context.Background(), []string{
		"--check-upstream", "--entry", "test", "--config", configPath, "--transport", "stdio", "--",
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--", "upstream",
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing upstream tool error, got %v", err)
	}
}

func TestValidateUpstreamAcceptsMatchingAllowlist(t *testing.T) {
	t.Setenv("MCP_FILTER_TEST_HELPER", "1")
	configPath := filepath.Join(t.TempDir(), ".mcp-filter.json")
	if err := os.WriteFile(configPath, []byte(`{"entries":{"test":{"allow":["visible"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validate(context.Background(), []string{
		"--check-upstream", "--entry", "test", "--config", configPath, "--transport", "stdio", "--",
		os.Args[0], "-test.run=TestMCPFilterHelperProcess", "--", "upstream",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPFilterHelperProcess(t *testing.T) {
	if os.Getenv("MCP_FILTER_TEST_HELPER") != "1" {
		return
	}
	args := helperArguments()
	if len(args) == 0 {
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "proxy":
		err = proxy(context.Background(), args[1:])
	case "upstream":
		err = runE2EUpstream(context.Background())
	default:
		err = fmt.Errorf("unknown helper command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func helperArguments() []string {
	for index, argument := range os.Args {
		if argument == "--" && index+1 < len(os.Args) {
			return os.Args[index+1:]
		}
	}
	return nil
}

func runE2EUpstream(ctx context.Context) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-upstream", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "visible", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments struct {
			Issue string `json:"issue"`
		}
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "forwarded:" + arguments.Issue}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "hidden", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "must not be exposed"}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "too late"}}}, nil
		}
	})
	server.AddPrompt(&mcp.Prompt{Name: "status", Description: "original prompt"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: mcp.Role("user"), Content: &mcp.TextContent{Text: "status"}}}}, nil
	})
	server.AddResource(&mcp.Resource{URI: "test://item", Name: "item", Description: "original resource"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "test://item", Text: "resource content"}}}, nil
	})
	return server.Run(ctx, &mcp.StdioTransport{})
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
