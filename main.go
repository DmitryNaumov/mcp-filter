package main

import (
	"context"
	"crypto/sha256"
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
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/fsnotify/fsnotify"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	version = "0.1.0-dev"
	usage   = `usage:
  mcp-filter SERVER [options] [-- UPSTREAM_COMMAND [args...]]
  mcp-filter validate SERVER [--config PATH]
  mcp-filter validate --check-upstream SERVER [options] [-- UPSTREAM_COMMAND [args...]]
  mcp-filter inspect SERVER [options] [-- UPSTREAM_COMMAND [args...]]
  mcp-filter version

SERVER is the MCP server name: a key in mcpServers in .mcp-filter.json.
Without that key, mcp-filter runs as a transparent pass-through.
validate checks rules syntax and the allowlist; --check-upstream also verifies
that allowed tools exist upstream. inspect lists tools that would be published.
version prints the mcp-filter version, source commit and commit date.`
)

var pseudoVersion = regexp.MustCompile(`[.-]([0-9]{14})-([0-9a-f]{12})(?:\+incompatible)?$`)

func main() {
	if len(os.Args) < 2 {
		fatal(usage)
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
		fmt.Println(versionDetails())
		return
	default:
		err = proxy(ctx, os.Args[1:])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func versionDetails() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return formatVersionDetails(nil)
	}
	return formatVersionDetails(info)
}

func formatVersionDetails(info *debug.BuildInfo) string {
	revision, commitDate, modified := "", "", false
	moduleVersion := ""
	if info != nil {
		moduleVersion = info.Main.Version
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.time":
				commitDate = setting.Value
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
		if (revision == "" || commitDate == "") && strings.HasPrefix(moduleVersion, "v") {
			if parts := pseudoVersion.FindStringSubmatch(moduleVersion); parts != nil {
				if date, err := time.Parse("20060102150405", parts[1]); err == nil {
					if revision == "" {
						revision = parts[2]
					}
					if commitDate == "" {
						commitDate = date.UTC().Format(time.RFC3339)
					}
				}
			}
		}
	}
	if revision == "" {
		revision = "unavailable"
	}
	if commitDate == "" {
		commitDate = "unavailable"
	}
	lines := []string{fmt.Sprintf("mcp-filter %s", version), "Commit: " + revision, "Commit date: " + commitDate}
	if modified {
		lines = append(lines, "Source modified: true")
	}
	if revision == "unavailable" && moduleVersion != "" && moduleVersion != "(devel)" {
		lines = append(lines, "Module version: "+moduleVersion)
	}
	return strings.Join(lines, "\n")
}

func proxy(ctx context.Context, args []string) (proxyErr error) {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	configPath, err := resolveConfigPath(opts.config)
	if err != nil {
		return err
	}
	cfg, err := config.LoadPath(configPath)
	if err != nil {
		return err
	}
	closeLogger, err := configureLogger(cfg, configPath, opts.entry)
	if err != nil {
		return err
	}
	defer closeLogger()
	defer func() {
		if proxyErr != nil {
			slog.Error("MCP proxy failed", "error", proxyErr)
		}
	}()
	slog.Info("MCP proxy starting", "transport", opts.transport)
	toolChanges := make(chan struct{}, 1)
	entry, configured, session, tools, err := connectWithConfig(ctx, opts, command, cfg, func(kind string) {
		if kind != "tools" {
			slog.Info("upstream MCP list changed; upstream refresh is pending implementation", "entry", opts.entry, "kind", kind)
			return
		}
		select {
		case toolChanges <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer session.Close()
	slog.Info("connected upstream", "transport", opts.transport, "configured", configured, "tool_count", len(tools))

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
	primitives, err := newPrimitiveState(ctx, server, session)
	if err != nil {
		return err
	}
	primitiveCatalog, err := primitives.prepare(entry)
	if err != nil {
		return err
	}
	state, err := newToolState(server, session, opts.entry, tools, entry, configured, opts.timeout)
	if err != nil {
		return err
	}
	primitives.apply(primitiveCatalog)
	state.publishInitial()
	go watchUpstreamTools(ctx, toolChanges, state)
	go watchRules(ctx, configPath, opts.entry, cfg, func(updated config.Entry, configured bool) error {
		prepared, err := primitives.prepare(updated)
		if err != nil {
			return err
		}
		if err := state.reload(updated, configured); err != nil {
			return err
		}
		primitives.apply(prepared)
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
	serverName := positionalServerName(&args)
	entryName := fs.String("entry", "", "MCP server name; prefer the first positional argument")
	configPath := fs.String("config", "", "path to .mcp-filter.json (defaults to upward search)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if serverName != "" {
		if *entryName != "" && *entryName != serverName {
			return fmt.Errorf("server name %q conflicts with --entry %q", serverName, *entryName)
		}
		*entryName = serverName
	}
	if *entryName == "" {
		return errors.New("MCP server name is required as the first argument or --entry")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	entry, err := cfg.Entry(*entryName)
	if err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return fmt.Errorf("MCP server %q: %w", *entryName, err)
	}
	fmt.Fprintf(os.Stdout, "MCP server %q is valid (%d allowed tools)\n", *entryName, len(entry.Allow))
	return nil
}

func validateUpstream(ctx context.Context, args []string) error {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(opts.config)
	if err != nil {
		return err
	}
	entry, configured := cfg.Lookup(opts.entry)
	_, _, session, tools, err := connectWithConfig(ctx, opts, command, cfg, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	available := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		available[tool.Name] = struct{}{}
	}
	var missing []string
	if !configured {
		return fmt.Errorf("MCP server %q is not configured", opts.entry)
	}
	for _, allowed := range entry.Allow {
		if _, ok := available[allowed]; !ok {
			missing = append(missing, allowed)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("MCP server %q allowlist references unavailable upstream tools: %s", opts.entry, strings.Join(missing, ", "))
	}
	fmt.Fprintf(os.Stdout, "MCP server %q matches upstream (%d allowed tools)\n", opts.entry, len(entry.Allow))
	return nil
}

func inspect(ctx context.Context, args []string) error {
	opts, command, err := parseProxyFlags(args)
	if err != nil {
		return err
	}
	entry, configured, session, tools, err := connect(ctx, opts, command)
	if err != nil {
		return err
	}
	defer session.Close()
	for _, tool := range tools {
		status := "hidden"
		if !configured || entry.Allowed(tool.Name) {
			status = "published"
		} else if !entry.Denied(tool.Name) {
			status = "discoverable"
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
	positionalEntry := positionalServerName(&args)
	fs := flag.NewFlagSet("mcp-filter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts proxyOptions
	fs.StringVar(&opts.entry, "entry", "", "MCP server name; prefer the first positional argument")
	fs.StringVar(&opts.config, "config", "", "path to .mcp-filter.json (defaults to upward search)")
	fs.StringVar(&opts.transport, "transport", "", "upstream transport: stdio, auto, streamable-http, or sse")
	fs.StringVar(&opts.url, "url", "", "upstream HTTP endpoint")
	fs.DurationVar(&opts.timeout, "timeout", 120*time.Second, "maximum duration of one upstream tool call; 0 disables the limit")
	fs.Var(&opts.headers, "header", "upstream HTTP header NAME=VALUE (repeatable)")
	fs.Var(&opts.headerEnv, "header-env", "upstream HTTP header from environment HEADER=ENVIRONMENT_VARIABLE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return proxyOptions{}, nil, err
	}
	if positionalEntry != "" {
		if opts.entry != "" && opts.entry != positionalEntry {
			return proxyOptions{}, nil, fmt.Errorf("MCP server name %q conflicts with positional name %q", opts.entry, positionalEntry)
		}
		opts.entry = positionalEntry
	}
	if opts.entry == "" {
		return proxyOptions{}, nil, errors.New("MCP server name is required as the first argument or --entry")
	}
	if opts.transport == "" {
		if opts.url != "" {
			opts.transport = "auto"
		} else {
			opts.transport = "stdio"
		}
	}
	switch opts.transport {
	case "stdio":
		if len(fs.Args()) == 0 {
			return proxyOptions{}, nil, errors.New("stdio requires an upstream command after --")
		}
	case "auto", "streamable-http", "sse":
		if opts.url == "" {
			return proxyOptions{}, nil, fmt.Errorf("%s requires --url", opts.transport)
		}
	default:
		return proxyOptions{}, nil, fmt.Errorf("unsupported transport %q", opts.transport)
	}
	return opts, fs.Args(), nil
}

func positionalServerName(args *[]string) string {
	if len(*args) == 0 || (*args)[0] == "--" || strings.HasPrefix((*args)[0], "-") {
		return ""
	}
	name := (*args)[0]
	*args = (*args)[1:]
	return name
}

func connect(ctx context.Context, opts proxyOptions, command []string) (config.Entry, bool, *mcp.ClientSession, []*mcp.Tool, error) {
	cfg, err := loadConfig(opts.config)
	if err != nil {
		return config.Entry{}, false, nil, nil, err
	}
	return connectWithConfig(ctx, opts, command, cfg, nil)
}

func connectWithConfig(ctx context.Context, opts proxyOptions, command []string, cfg config.Config, onChange func(string)) (config.Entry, bool, *mcp.ClientSession, []*mcp.Tool, error) {
	entry, configured := cfg.EffectiveEntry(opts.entry)
	if configured {
		if err := entry.Validate(); err != nil {
			return config.Entry{}, false, nil, nil, fmt.Errorf("entry %q: %w", opts.entry, err)
		}
	}
	client := newUpstreamClient(onChange)
	transport, err := newTransport(opts, command)
	if err != nil {
		return config.Entry{}, false, nil, nil, err
	}
	session, tools, err := connectTools(ctx, client, transport)
	if err != nil && opts.transport == "auto" && isProtocolIncompatibility(err) && isLegacySSEEndpoint(ctx, opts) {
		session, tools, err = connectTools(ctx, client, &mcp.SSEClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)})
	}
	if err != nil {
		return config.Entry{}, false, nil, nil, fmt.Errorf("connect upstream %q: %w", opts.entry, err)
	}
	return entry, configured, session, tools, nil
}

func isProtocolIncompatibility(err error) bool {
	// The SDK includes the HTTP status text in errors returned while sending
	// initialize. Only statuses that commonly express a transport mismatch are
	// candidates; the legacy handshake below must still positively verify SSE.
	message := err.Error()
	for _, status := range []string{
		"Bad Request", "Not Found", "Method Not Allowed", "Not Acceptable", "Unsupported Media Type",
	} {
		if strings.Contains(message, status) {
			return true
		}
	}
	return false
}

func connectTools(ctx context.Context, client *mcp.Client, transport mcp.Transport) (*mcp.ClientSession, []*mcp.Tool, error) {
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, nil, err
	}
	return session, result.Tools, nil
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

func watchRules(ctx context.Context, basePath, entryName string, initial config.Config, apply func(config.Entry, bool) error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Default().Error("start rules watcher", "error", err)
		watchRulesLoop(ctx, basePath, entryName, initial, nil, nil, time.Second, time.Minute, apply)
		return
	}
	directory := filepath.Dir(basePath)
	if err := watcher.Add(directory); err != nil {
		slog.Default().Error("watch rules directory", "error", err)
		watcher.Close()
		watchRulesLoop(ctx, basePath, entryName, initial, nil, nil, time.Second, time.Minute, apply)
		return
	}
	defer watcher.Close()
	watchRulesLoop(ctx, basePath, entryName, initial, watcher.Events, watcher.Errors, time.Second, time.Minute, apply)
}

type rulesHash struct {
	base         [32]byte
	local        [32]byte
	localPresent bool
}

func readRulesHash(basePath string) (rulesHash, error) {
	base, err := os.ReadFile(basePath)
	if err != nil {
		return rulesHash{}, err
	}
	result := rulesHash{base: sha256.Sum256(base)}
	local, err := os.ReadFile(filepath.Join(filepath.Dir(basePath), config.LocalFile))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return rulesHash{}, err
	}
	result.local = sha256.Sum256(local)
	result.localPresent = true
	return result, nil
}

func watchRulesLoop(ctx context.Context, basePath, entryName string, initial config.Config, events <-chan fsnotify.Event, watcherErrors <-chan error, debounce, pollInterval time.Duration, apply func(config.Entry, bool) error) {
	lastEntry, lastConfigured := initial.EffectiveEntry(entryName)
	var lastHash rulesHash
	hashKnown := false
	check := func() {
		hash, err := readRulesHash(basePath)
		if err != nil {
			slog.Default().Error("read rules content", "path", basePath, "error", err)
			return
		}
		if hashKnown && hash == lastHash {
			return
		}
		cfg, err := config.LoadPath(basePath)
		if err != nil {
			slog.Default().Error("reload rules", "path", basePath, "error", err)
			return
		}
		entry, configured := cfg.EffectiveEntry(entryName)
		if configured != lastConfigured || !reflect.DeepEqual(entry, lastEntry) {
			if configured {
				if err := entry.Validate(); err != nil {
					slog.Default().Error("reload rules entry invalid", "entry", entryName, "error", err)
					return
				}
			}
			if err := apply(entry, configured); err != nil {
				slog.Default().Error("apply reloaded rules", "entry", entryName, "error", err)
				return
			}
			lastEntry, lastConfigured = entry, configured
			slog.Default().Info("reloaded MCP filter rules", "entry", entryName)
		}
		lastHash, hashKnown = hash, true
	}
	// Reconcile a change that happened between the initial load and subscription.
	check()
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	pending := false
	baseName := filepath.Base(basePath)
	allErrors := watcherErrors
	queueCheck := func() {
		timer.Stop()
		timer.Reset(debounce)
		pending = true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			name := filepath.Base(event.Name)
			if name != baseName && name != config.LocalFile {
				continue
			}
			// Each new event starts a fresh quiet period.
			queueCheck()
		case err, ok := <-watcherErrors:
			if !ok {
				watcherErrors, allErrors = nil, nil
				continue
			}
			slog.Default().Error("watch rules", "error", err)
			// Fall back to the hash poll if a faulty watcher floods errors.
			watcherErrors = nil
		case <-timer.C:
			pending = false
			check()
		case <-poll.C:
			watcherErrors = allErrors
			if !pending {
				hash, err := readRulesHash(basePath)
				if err != nil {
					slog.Default().Error("read rules content", "path", basePath, "error", err)
				} else if !hashKnown || hash != lastHash {
					queueCheck()
				}
			}
		}
	}
}

func newTransport(opts proxyOptions, command []string) (mcp.Transport, error) {
	switch opts.transport {
	case "stdio":
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stderr = os.Stderr
		return &normalizedCommandTransport{command: cmd}, nil
	case "streamable-http":
		return &mcp.StreamableClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)}, nil
	case "auto":
		return &mcp.StreamableClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)}, nil
	case "sse":
		return &mcp.SSEClientTransport{Endpoint: opts.url, HTTPClient: httpClient(opts.headers, opts.headerEnv)}, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", opts.transport)
	}
}

// isLegacySSEEndpoint verifies the old SSE handshake before auto mode falls
// back. It prevents a failed streamable request caused by authentication,
// DNS, timeouts, or an ordinary server error from silently trying another
// protocol. The probe's temporary session is closed immediately.
func isLegacySSEEndpoint(ctx context.Context, opts proxyOptions) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.url, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := httpClient(opts.headers, opts.headerEnv).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return false
	}
	// The legacy protocol starts with an endpoint event. Read only enough of
	// the response to identify it; do not treat a generic SSE stream as MCP.
	buf := make([]byte, 4096)
	n, err := response.Body.Read(buf)
	if err != nil && n == 0 {
		return false
	}
	return strings.Contains(string(buf[:n]), "event: endpoint")
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
