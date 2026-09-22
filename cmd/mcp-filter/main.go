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
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcecraft/mcp-filter/internal/config"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		fatal("usage: mcp-filter <entry> [options] [-- upstream-command] | <validate|inspect|version>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "validate":
		err = validate(ctx, os.Args[2:])
	case "inspect":
		err = inspect(ctx, os.Args[2:])
	case "version":
		fmt.Println(version)
		return
	default:
		err = proxy(ctx, os.Args[1:])
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
	configPath, err := resolveConfigPath(opts.config)
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
	server := mcp.NewServer(implementation, &mcp.ServerOptions{
		Logger:       slog.Default(),
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{
			Tools:     &mcp.ToolCapabilities{ListChanged: true},
			Prompts:   &mcp.PromptCapabilities{ListChanged: true},
			Resources: &mcp.ResourceCapabilities{ListChanged: true},
		},
	})
	if err := attachPassthroughPrimitives(ctx, server, session, entry); err != nil {
		return err
	}
	filteredTools, err := prepareAllowedTools(tools, entry)
	if err != nil {
		return err
	}
	addTools(server, session, filteredTools, opts.timeout)
	go watchRules(ctx, configPath, opts.entry, func(updated config.Entry) error {
		filteredTools, err := prepareAllowedTools(tools, updated)
		if err != nil {
			return err
		}
		server.RemoveTools(toolNames(tools)...)
		addTools(server, session, filteredTools, opts.timeout)
		return nil
	})
	return server.Run(ctx, &mcp.StdioTransport{})
}

func validate(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "--check-upstream" {
		return validateUpstream(ctx, args[1:])
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
	timeout   time.Duration
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
	var positionalEntry string
	if len(args) > 0 && args[0] != "--" && !strings.HasPrefix(args[0], "-") {
		positionalEntry = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("mcp-filter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts proxyOptions
	fs.StringVar(&opts.entry, "entry", "", "configured entry name")
	fs.StringVar(&opts.config, "config", "", "path to .mcp-filter.json (defaults to upward search)")
	fs.StringVar(&opts.transport, "transport", "stdio", "upstream transport: stdio, streamable-http, or sse")
	fs.StringVar(&opts.url, "url", "", "upstream HTTP endpoint")
	fs.DurationVar(&opts.timeout, "timeout", 120*time.Second, "maximum duration of one upstream tool call; 0 disables the limit")
	fs.Var(&opts.headers, "header", "upstream HTTP header NAME=VALUE (repeatable)")
	fs.Var(&opts.headerEnv, "header-env", "upstream HTTP header from environment HEADER=ENVIRONMENT_VARIABLE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return proxyOptions{}, nil, err
	}
	if positionalEntry != "" {
		if opts.entry != "" && opts.entry != positionalEntry {
			return proxyOptions{}, nil, fmt.Errorf("entry %q conflicts with positional entry %q", opts.entry, positionalEntry)
		}
		opts.entry = positionalEntry
	}
	if opts.entry == "" {
		return proxyOptions{}, nil, errors.New("entry name is required as the first argument or --entry")
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
	client := newUpstreamClient(func(kind string) {
		slog.Default().Info("upstream MCP list changed; upstream refresh is pending implementation", "entry", opts.entry, "kind", kind)
	})
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

func newUpstreamClient(onChange func(kind string)) *mcp.Client {
	notify := func(kind string) {
		if onChange != nil {
			onChange(kind)
		}
	}
	return mcp.NewClient(&mcp.Implementation{Name: "mcp-filter", Version: version}, &mcp.ClientOptions{
		Logger: slog.Default(),
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			notify("tools")
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			notify("prompts")
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			notify("resources")
		},
	})
}

func loadConfig(path string) (config.Config, error) {
	if path != "" {
		return config.LoadPath(path)
	}
	return config.Load(".")
}

func resolveConfigPath(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	return config.ResolvePath(".")
}

func prepareAllowedTools(tools []*mcp.Tool, entry config.Entry) ([]*mcp.Tool, error) {
	filtered := make([]*mcp.Tool, 0, len(tools))
	for _, upstreamTool := range tools {
		if !entry.Allowed(upstreamTool.Name) {
			continue
		}
		patch, err := toolMetadataPatch(entry, upstreamTool)
		if err != nil {
			return nil, fmt.Errorf("select metadata for tool %q: %w", upstreamTool.Name, err)
		}
		tool, err := overlayTool(upstreamTool, patch)
		if err != nil {
			return nil, fmt.Errorf("apply metadata for tool %q: %w", upstreamTool.Name, err)
		}
		filtered = append(filtered, tool)
	}
	return filtered, nil
}

func addTools(server *mcp.Server, session *mcp.ClientSession, tools []*mcp.Tool, timeout time.Duration) {
	for _, tool := range tools {
		name := tool.Name
		server.AddTool(tool, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments any = map[string]any{}
			if len(request.Params.Arguments) != 0 {
				if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
					return nil, fmt.Errorf("decode tool arguments: %w", err)
				}
			}
			if timeout > 0 {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(callCtx, timeout)
				defer cancel()
			}
			return session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		})
	}
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func watchRules(ctx context.Context, basePath, entryName string, apply func(config.Entry) error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Default().Error("start rules watcher", "error", err)
		return
	}
	defer watcher.Close()
	directory := filepath.Dir(basePath)
	if err := watcher.Add(directory); err != nil {
		slog.Default().Error("watch rules directory", "error", err)
		return
	}
	baseName := filepath.Base(basePath)
	pending := false
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	queueReload := func() {
		if pending {
			return
		}
		pending = true
		timer.Reset(100 * time.Millisecond)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			name := filepath.Base(event.Name)
			if name == baseName || name == config.LocalFile {
				queueReload()
			}
		case err, ok := <-watcher.Errors:
			if ok {
				slog.Default().Error("watch rules", "error", err)
			}
		case <-timer.C:
			pending = false
			cfg, err := config.LoadPath(basePath)
			if err != nil {
				slog.Default().Error("reload rules", "path", basePath, "error", err)
				continue
			}
			entry, err := cfg.Entry(entryName)
			if err != nil {
				slog.Default().Error("reload rules entry", "entry", entryName, "error", err)
				continue
			}
			if entry.Allow == nil {
				slog.Default().Error("reload rules entry has no allowlist", "entry", entryName)
				continue
			}
			if err := apply(entry); err != nil {
				slog.Default().Error("apply reloaded rules", "entry", entryName, "error", err)
				continue
			}
			slog.Default().Info("reloaded MCP filter rules", "entry", entryName)
		}
	}
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

func toolMetadataPatch(entry config.Entry, tool *mcp.Tool) (map[string]any, error) {
	patch := config.MergePatch(nil, entry.Metadata.Tools[tool.Name])
	encoded, err := json.Marshal(tool)
	if err != nil {
		return nil, err
	}
	var candidate map[string]any
	if err := json.Unmarshal(encoded, &candidate); err != nil {
		return nil, err
	}
	for _, genericPatch := range entry.Metadata.Patches {
		if genericPatch.Method != "tools/list" || !matchesSelector(candidate, genericPatch.Select) {
			continue
		}
		patch = config.MergePatch(patch, genericPatch.Patch)
	}
	return patch, nil
}

func matchesSelector(candidate, selector map[string]any) bool {
	for key, expected := range selector {
		actual, ok := candidate[key]
		if !ok || !reflect.DeepEqual(actual, expected) {
			return false
		}
	}
	return true
}

func attachPassthroughPrimitives(ctx context.Context, server *mcp.Server, session *mcp.ClientSession, entry config.Entry) error {
	initialized := session.InitializeResult()
	if initialized == nil || initialized.Capabilities == nil {
		return nil
	}
	if initialized.Capabilities.Prompts != nil {
		prompts, err := session.ListPrompts(ctx, nil)
		if err != nil {
			return fmt.Errorf("list upstream prompts: %w", err)
		}
		for _, upstreamPrompt := range prompts.Prompts {
			prompt, err := overlayMetadata("prompts/list", upstreamPrompt, entry.Metadata.Patches)
			if err != nil {
				return fmt.Errorf("apply metadata for prompt %q: %w", upstreamPrompt.Name, err)
			}
			name := upstreamPrompt.Name
			server.AddPrompt(prompt, func(callCtx context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				params := *request.Params
				params.Name = name
				return session.GetPrompt(callCtx, &params)
			})
		}
	}
	if initialized.Capabilities.Resources != nil {
		resources, err := session.ListResources(ctx, nil)
		if err != nil {
			return fmt.Errorf("list upstream resources: %w", err)
		}
		for _, upstreamResource := range resources.Resources {
			resource, err := overlayMetadata("resources/list", upstreamResource, entry.Metadata.Patches)
			if err != nil {
				return fmt.Errorf("apply metadata for resource %q: %w", upstreamResource.URI, err)
			}
			server.AddResource(resource, func(callCtx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				params := *request.Params
				return session.ReadResource(callCtx, &params)
			})
		}
		templates, err := session.ListResourceTemplates(ctx, nil)
		if err != nil {
			return fmt.Errorf("list upstream resource templates: %w", err)
		}
		for _, upstreamTemplate := range templates.ResourceTemplates {
			template, err := overlayMetadata("resources/templates/list", upstreamTemplate, entry.Metadata.Patches)
			if err != nil {
				return fmt.Errorf("apply metadata for resource template %q: %w", upstreamTemplate.URITemplate, err)
			}
			server.AddResourceTemplate(template, func(callCtx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				params := *request.Params
				return session.ReadResource(callCtx, &params)
			})
		}
	}
	return nil
}

func overlayMetadata[T any](method string, value *T, patches []config.Patch) (*T, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var candidate map[string]any
	if err := json.Unmarshal(encoded, &candidate); err != nil {
		return nil, err
	}
	for _, patch := range patches {
		if patch.Method == method && matchesSelector(candidate, patch.Select) {
			candidate = config.MergePatch(candidate, patch.Patch)
		}
	}
	encoded, err = json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
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
