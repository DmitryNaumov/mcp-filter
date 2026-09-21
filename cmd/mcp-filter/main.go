package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcecraft/mcp-filter/internal/config"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		fatal("usage: mcp-filter <proxy|validate|inspect|version>")
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "proxy":
		err = proxy(ctx, os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "inspect":
		err = inspect(ctx, os.Args[2:])
	case "version":
		fmt.Println(version)
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func proxy(ctx context.Context, args []string) error {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	entry, session, tools, err := connect(ctx, opts, command)
	if err != nil {
		return err
	}
	defer session.Close()

	implementation, instructions, err := overlayServer(opts.entry, session, entry.Metadata.Server)
	if err != nil {
		return fmt.Errorf("apply server metadata: %w", err)
	}
	server := mcp.NewServer(implementation, &mcp.ServerOptions{Instructions: instructions, Logger: slog.Default()})
	for _, upstreamTool := range tools {
		if !entry.Allowed(upstreamTool.Name) {
			continue
		}
		tool, err := overlayTool(upstreamTool, entry.Metadata.Tools[upstreamTool.Name])
		if err != nil {
			return fmt.Errorf("apply metadata for tool %q: %w", upstreamTool.Name, err)
		}
		name := upstreamTool.Name
		server.AddTool(tool, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments any = map[string]any{}
			if len(request.Params.Arguments) != 0 {
				if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
					return nil, fmt.Errorf("decode tool arguments: %w", err)
				}
			}
			return session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		})
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func validate(args []string) error {
	if len(args) > 0 && args[0] == "--check-upstream" {
		return validateUpstream(context.Background(), args[1:])
	}
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	entryName := fs.String("entry", "", "configured entry name")
	configPath := fs.String("config", "", "path to .mcp-filter.json (defaults to upward search)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *entryName == "" {
		return errors.New("--entry is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	entry, err := cfg.Entry(*entryName)
	if err != nil {
		return err
	}
	if entry.Allow == nil {
		return fmt.Errorf("entry %q must declare allow explicitly", *entryName)
	}
	fmt.Fprintf(os.Stdout, "entry %q is valid (%d allowed tools)\n", *entryName, len(entry.Allow))
	return nil
}

func validateUpstream(ctx context.Context, args []string) error {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	entry, session, tools, err := connect(ctx, opts, command)
	if err != nil {
		return err
	}
	defer session.Close()
	available := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		available[tool.Name] = struct{}{}
	}
	var missing []string
	for _, allowed := range entry.Allow {
		if _, ok := available[allowed]; !ok {
			missing = append(missing, allowed)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("entry %q allowlist references unavailable upstream tools: %s", opts.entry, strings.Join(missing, ", "))
	}
	fmt.Fprintf(os.Stdout, "entry %q matches upstream (%d allowed tools)\n", opts.entry, len(entry.Allow))
	return nil
}

func inspect(ctx context.Context, args []string) error {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	entry, session, tools, err := connect(ctx, opts, command)
	if err != nil {
		return err
	}
	defer session.Close()
	for _, tool := range tools {
		status := "hidden"
		if entry.Allowed(tool.Name) {
			status = "published"
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\n", status, tool.Name)
	}
	return nil
}

type proxyOptions struct {
	entry     string
	config    string
	transport string
	url       string
	headers   headerFlags
	headerEnv headerEnvFlags
}

type headerFlags []string

type headerEnvFlags []string

func (h *headerFlags) String() string { return strings.Join(*h, ",") }
func (h *headerFlags) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("header must be NAME=VALUE")
	}
	*h = append(*h, value)
	return nil
}

func (h *headerEnvFlags) String() string { return strings.Join(*h, ",") }
func (h *headerEnvFlags) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("header-env must be HEADER=ENVIRONMENT_VARIABLE")
	}
	*h = append(*h, value)
	return nil
}

func parseProxyFlags(args []string) (proxyOptions, []string, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts proxyOptions
	fs.StringVar(&opts.entry, "entry", "", "configured entry name")
	fs.StringVar(&opts.config, "config", "", "path to .mcp-filter.json (defaults to upward search)")
	fs.StringVar(&opts.transport, "transport", "stdio", "upstream transport: stdio, streamable-http, or sse")
	fs.StringVar(&opts.url, "url", "", "upstream HTTP endpoint")
	fs.Var(&opts.headers, "header", "upstream HTTP header NAME=VALUE (repeatable)")
	fs.Var(&opts.headerEnv, "header-env", "upstream HTTP header from environment HEADER=ENVIRONMENT_VARIABLE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return proxyOptions{}, nil, err
	}
	if opts.entry == "" {
		return proxyOptions{}, nil, errors.New("--entry is required")
	}
	switch opts.transport {
	case "stdio":
		if len(fs.Args()) == 0 {
			return proxyOptions{}, nil, errors.New("stdio requires an upstream command after --")
		}
	case "streamable-http", "sse":
		if opts.url == "" {
			return proxyOptions{}, nil, fmt.Errorf("%s requires --url", opts.transport)
		}
	default:
		return proxyOptions{}, nil, fmt.Errorf("unsupported transport %q", opts.transport)
	}
	return opts, fs.Args(), nil
}

func connect(ctx context.Context, opts proxyOptions, command []string) (config.Entry, *mcp.ClientSession, []*mcp.Tool, error) {
	cfg, err := loadConfig(opts.config)
	if err != nil {
		return config.Entry{}, nil, nil, err
	}
	entry, err := cfg.Entry(opts.entry)
	if err != nil {
		return config.Entry{}, nil, nil, err
	}
	if entry.Allow == nil {
		return config.Entry{}, nil, nil, fmt.Errorf("entry %q must declare allow explicitly", opts.entry)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-filter", Version: version}, &mcp.ClientOptions{Logger: slog.Default()})
	transport, err := newTransport(opts, command)
	if err != nil {
		return config.Entry{}, nil, nil, err
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return config.Entry{}, nil, nil, fmt.Errorf("connect upstream %q: %w", opts.entry, err)
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return config.Entry{}, nil, nil, fmt.Errorf("list upstream tools for %q: %w", opts.entry, err)
	}
	return entry, session, result.Tools, nil
}

func loadConfig(path string) (config.Config, error) {
	if path != "" {
		return config.LoadPath(path)
	}
	return config.Load(".")
}

func newTransport(opts proxyOptions, command []string) (mcp.Transport, error) {
	switch opts.transport {
	case "stdio":
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stderr = os.Stderr
		return &mcp.CommandTransport{Command: cmd}, nil
	case "streamable-http":
		return &mcp.StreamableClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)}, nil
	case "sse":
		return &mcp.SSEClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)}, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", opts.transport)
	}
}

func httpClient(headers headerFlags, headerEnv headerEnvFlags) *http.Client {
	return &http.Client{Transport: headerTransport{headers: headers, headerEnv: headerEnv, next: http.DefaultTransport}}
}

type headerTransport struct {
	headers   headerFlags
	headerEnv headerEnvFlags
	next      http.RoundTripper
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	for _, header := range t.headers {
		name, value, _ := strings.Cut(header, "=")
		copy.Header.Set(name, value)
	}
	for _, header := range t.headerEnv {
		name, environment, _ := strings.Cut(header, "=")
		if value, ok := os.LookupEnv(environment); ok {
			copy.Header.Set(name, value)
		}
	}
	return t.next.RoundTrip(copy)
}

func overlayTool(tool *mcp.Tool, patch map[string]any) (*mcp.Tool, error) {
	if patch == nil {
		copy := *tool
		return &copy, nil
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		return nil, err
	}
	var source map[string]any
	if err := json.Unmarshal(encoded, &source); err != nil {
		return nil, err
	}
	merged, err := json.Marshal(config.MergePatch(source, patch))
	if err != nil {
		return nil, err
	}
	var result mcp.Tool
	if err := json.Unmarshal(merged, &result); err != nil {
		return nil, err
	}
	if result.Name != tool.Name {
		return nil, errors.New("metadata patch cannot change tool name")
	}
	return &result, nil
}

func overlayServer(entryName string, session *mcp.ClientSession, patch map[string]any) (*mcp.Implementation, string, error) {
	implementation := mcp.Implementation{Name: entryName, Version: version}
	instructions := ""
	if session != nil {
		initialized := session.InitializeResult()
		if initialized != nil {
			if initialized.ServerInfo != nil {
				implementation = *initialized.ServerInfo
			}
			instructions = initialized.Instructions
		}
	}
	if patch == nil {
		return &implementation, instructions, nil
	}
	encoded, err := json.Marshal(implementation)
	if err != nil {
		return nil, "", err
	}
	var source map[string]any
	if err := json.Unmarshal(encoded, &source); err != nil {
		return nil, "", err
	}
	merged, err := json.Marshal(config.MergePatch(source, patch))
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(merged, &implementation); err != nil {
		return nil, "", err
	}
	if implementation.Name != source["name"] {
		return nil, "", errors.New("metadata patch cannot change server name")
	}
	if value, ok := patch["instructions"]; ok {
		text, ok := value.(string)
		if !ok {
			return nil, "", errors.New("instructions must be a string")
		}
		instructions = text
	}
	return &implementation, instructions, nil
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "mcp-filter:", message)
	os.Exit(1)
}
