package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var helperNames = []string{"search_tools", "describe_tool", "call_tool"}

type toolState struct {
	mu         sync.Mutex
	server     *mcp.Server
	session    *mcp.ClientSession
	entryName  string
	upstream   []*mcp.Tool
	tools      map[string]*mcp.Tool
	entry      config.Entry
	configured bool
	active     map[string]bool
	published  map[string]bool
	timeout    time.Duration
}

func newToolState(server *mcp.Server, session *mcp.ClientSession, entryName string, upstream []*mcp.Tool, entry config.Entry, configured bool, timeout time.Duration) (*toolState, error) {
	s := &toolState{server: server, session: session, entryName: entryName, upstream: upstream, entry: entry, configured: configured, timeout: timeout, active: map[string]bool{}, published: map[string]bool{}}
	if err := s.prepare(entry, configured); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *toolState) prepare(entry config.Entry, configured bool) error {
	if configured {
		if err := entry.Validate(); err != nil {
			return err
		}
	}
	tools := make(map[string]*mcp.Tool, len(s.upstream))
	for _, original := range s.upstream {
		if configured {
			for _, helper := range helperNames {
				if original.Name == helper {
					return fmt.Errorf("upstream tool %q conflicts with auto helper", helper)
				}
			}
		}
		tool := original
		if configured {
			patch, err := toolMetadataPatch(entry, original)
			if err != nil {
				return err
			}
			tool, err = overlayTool(original, patch)
			if err != nil {
				return fmt.Errorf("apply metadata for tool %q: %w", original.Name, err)
			}
		}
		if err := validateToolSchemas(tool); err != nil {
			return fmt.Errorf("tool %q: %w", original.Name, err)
		}
		tools[original.Name] = tool
	}
	s.tools = tools
	return nil
}

func validateToolSchemas(tool *mcp.Tool) error {
	for _, schema := range []struct {
		name  string
		value any
	}{{"input schema", tool.InputSchema}, {"output schema", tool.OutputSchema}} {
		if schema.value == nil && schema.name == "output schema" {
			continue
		}
		encoded, err := json.Marshal(schema.value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", schema.name, err)
		}
		var object map[string]any
		if err := json.Unmarshal(encoded, &object); err != nil {
			return fmt.Errorf("decode %s: %w", schema.name, err)
		}
		if object["type"] != "object" {
			return fmt.Errorf("%s must have type object", schema.name)
		}
	}
	return nil
}

func (s *toolState) visible(name string) bool {
	if !s.configured {
		return true
	}
	if s.entry.Denied(name) {
		return false
	}
	return s.entry.Allowed(name) || s.active[name]
}

func (s *toolState) candidate(name string) bool {
	if _, ok := s.tools[name]; !ok {
		return false
	}
	if !s.configured {
		return true
	}
	if s.entry.Denied(name) {
		return false
	}
	return true
}

func (s *toolState) publishInitial() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, original := range s.upstream {
		if s.visible(original.Name) {
			s.publish(original.Name)
		}
	}
	if s.configured {
		s.addHelpers()
	}
}

func (s *toolState) publish(name string) {
	if s.published[name] {
		return
	}
	tool := s.tools[name]
	s.server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args any = map[string]any{}
		if len(request.Params.Arguments) > 0 {
			if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("decode tool arguments: %w", err)
			}
		}
		return s.forward(ctx, name, args, false, "direct_call")
	})
	s.published[name] = true
}

// Authorization (and generic argument validation) uses one policy snapshot.
// The lock is released before the upstream call, so a call authorized just
// before a reload may still dispatch or finish afterward.
func (s *toolState) forward(ctx context.Context, name string, args any, validate bool, trigger string) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	if !s.candidate(name) || !s.visible(name) {
		s.mu.Unlock()
		slog.Warn("tool call rejected", "tool", name, "trigger", trigger)
		return nil, fmt.Errorf("tool %q is not allowed", name)
	}
	if validate {
		arguments, ok := args.(map[string]any)
		if !ok {
			s.mu.Unlock()
			return nil, fmt.Errorf("tool %q arguments must be an object", name)
		}
		if err := validateArguments(s.tools[name].InputSchema, arguments); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("invalid arguments for %q: %w", name, err)
		}
	}
	s.mu.Unlock()
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	result, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		slog.Error("upstream tool call failed", "entry", s.entryName, "tool", name, "trigger", trigger, "error", err)
	} else if result != nil && result.IsError {
		slog.Error("upstream tool returned error", "entry", s.entryName, "tool", name, "trigger", trigger)
	}
	return result, err
}

func (s *toolState) activate(name, trigger string) (*mcp.Tool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.candidate(name) {
		slog.Warn("tool request rejected", "tool", name, "trigger", trigger)
		return nil, fmt.Errorf("tool %q is unavailable", name)
	}
	if !s.visible(name) {
		s.active[name] = true
		s.publish(name)
		slog.Info("auto tool activated", "tool", name, "trigger", trigger)
	}
	return s.tools[name], nil
}

func (s *toolState) reload(entry config.Entry, configured bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldTools, oldConfigured := s.tools, s.configured
	if err := s.prepare(entry, configured); err != nil {
		s.tools = oldTools
		return err
	}
	s.entry, s.configured = entry, configured
	for name := range s.active {
		if !configured || !s.candidate(name) {
			delete(s.active, name)
		}
	}
	s.reconcilePublished(oldTools)
	if oldConfigured && !configured {
		s.server.RemoveTools(helperNames...)
	}
	if configured && !oldConfigured {
		s.addHelpers()
	}
	return nil
}

func (s *toolState) refreshUpstream(ctx context.Context) error {
	var upstream []*mcp.Tool
	for tool, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("list upstream tools: %w", err)
		}
		upstream = append(upstream, tool)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldUpstream, oldTools := s.upstream, s.tools
	s.upstream = upstream
	if err := s.prepare(s.entry, s.configured); err != nil {
		s.upstream, s.tools = oldUpstream, oldTools
		return fmt.Errorf("prepare upstream tools: %w", err)
	}
	for name := range s.active {
		if !s.candidate(name) {
			delete(s.active, name)
		}
	}
	s.reconcilePublished(oldTools)
	return nil
}

// reconcilePublished runs with s.mu held, after the new catalog is prepared.
func (s *toolState) reconcilePublished(oldTools map[string]*mcp.Tool) {
	for name := range s.published {
		if !s.candidate(name) || !s.visible(name) {
			s.server.RemoveTools(name)
			delete(s.published, name)
		}
	}
	for _, original := range s.upstream {
		name := original.Name
		if s.visible(name) {
			if s.published[name] && !sameTool(oldTools[name], s.tools[name]) {
				s.server.RemoveTools(name)
				delete(s.published, name)
			}
			s.publish(name)
		}
	}
}

func watchUpstreamTools(ctx context.Context, changes <-chan struct{}, state *toolState) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
			if err := state.refreshUpstream(ctx); err != nil && ctx.Err() == nil {
				slog.Error("refresh upstream tools", "entry", state.entryName, "error", err)
			}
		}
	}
}

func sameTool(a, b *mcp.Tool) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func helperTool(name, description string, properties map[string]any, required ...string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}

func (s *toolState) addHelpers() {
	s.server.AddTool(helperTool("search_tools", `Search this server's available tools, including tools hidden from the visible tool list. When you need a capability you cannot see, call with a few short alternatives, for example {"keywords":["issue","task"]}. A tool matches if at least one keyword occurs in its name, title, or description (case-insensitive literal substring). Returns up to 10 names, summaries, and activation states; [] means no match. Searching does not activate tools. Pass a result's exact name to describe_tool to get its input schema before calling it.`, map[string]any{
		"keywords": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string", "minLength": 1},
			"minItems":    1,
			"description": "Required short alternative words likely in the hidden tool's name or description. Each word is searched independently; any match qualifies.",
			"examples":    []any{[]string{"issue", "task"}},
		},
	}, "keywords"), s.search)
	s.server.AddTool(helperTool("describe_tool", `Get the complete metadata and input schema of a tool found with search_tools. Pass its exact name, for example {"name":"get_issue"}. This activates the tool for this connection. Read the returned schema before using call_tool; some clients do not immediately show activated tools in their tool list.`, map[string]any{
		"name": map[string]any{"type": "string", "minLength": 1, "description": "Exact name returned by search_tools."},
	}, "name"), s.describe)
	s.server.AddTool(helperTool("call_tool", `Invoke a tool by its exact name, including a hidden tool found with search_tools. First use describe_tool to learn its input schema. Then pass the result's name and an arguments object matching that schema. Calling a hidden tool activates it. This works even if the client does not show newly activated tools directly.`, map[string]any{
		"name":      map[string]any{"type": "string", "minLength": 1, "description": "Exact tool name returned by search_tools or describe_tool."},
		"arguments": map[string]any{"type": "object", "description": "Arguments required by the selected tool's input schema. Use {} only when that schema permits it."},
	}, "name", "arguments"), s.call)
}

func decodeHelper(request *mcp.CallToolRequest, target any) error {
	return json.Unmarshal(request.Params.Arguments, target)
}

func textResult(value any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func (s *toolState) search(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var input struct {
		Keywords []string `json:"keywords"`
	}
	if err := decodeHelper(request, &input); err != nil {
		return nil, err
	}
	slog.Info("helper tool called", "entry", s.entryName, "tool", "search_tools", "keywords", input.Keywords)
	if len(input.Keywords) == 0 {
		return nil, fmt.Errorf("keywords must contain at least one search word, for example {\"keywords\":[\"issue\",\"task\"]}")
	}
	for i, keyword := range input.Keywords {
		input.Keywords[i] = strings.ToLower(strings.TrimSpace(keyword))
		if input.Keywords[i] == "" {
			return nil, fmt.Errorf("keywords must not contain empty words")
		}
	}
	type hit struct {
		Name      string `json:"name"`
		Summary   string `json:"summary"`
		Activated bool   `json:"activated"`
		score     int
	}
	s.mu.Lock()
	var hits []hit
	for name, tool := range s.tools {
		if !s.candidate(name) {
			continue
		}
		haystack := strings.ToLower(name + " " + tool.Title + " " + tool.Description)
		score := 0
		for _, keyword := range input.Keywords {
			if !strings.Contains(haystack, keyword) {
				continue
			}
			matchScore := 1
			if strings.Contains(strings.ToLower(name), keyword) {
				matchScore = 2
			}
			if strings.EqualFold(name, keyword) {
				matchScore = 3
			}
			if matchScore > score {
				score = matchScore
			}
		}
		if score == 0 {
			continue
		}
		summary := strings.TrimSpace(tool.Description)
		if runes := []rune(summary); len(runes) > 160 {
			summary = string(runes[:160])
		}
		hits = append(hits, hit{Name: name, Summary: summary, Activated: s.visible(name), score: score})
	}
	s.mu.Unlock()
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Name < hits[j].Name
	})
	if len(hits) > 10 {
		hits = hits[:10]
	}
	if hits == nil {
		hits = []hit{}
	}
	result, err := textResult(hits)
	if err != nil {
		return nil, err
	}
	slog.Info("search_tools result", "entry", s.entryName, "result", result.Content[0].(*mcp.TextContent).Text)
	return result, nil
}

func (s *toolState) describe(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeHelper(request, &input); err != nil {
		return nil, err
	}
	slog.Info("helper tool called", "entry", s.entryName, "tool", "describe_tool", "target", input.Name)
	tool, err := s.activate(input.Name, "describe_tool")
	if err != nil {
		return nil, err
	}
	return textResult(tool)
}

func (s *toolState) call(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var input struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decodeHelper(request, &input); err != nil {
		return nil, err
	}
	slog.Info("helper tool called", "entry", s.entryName, "tool", "call_tool", "target", input.Name)
	if input.Arguments == nil {
		input.Arguments = map[string]any{}
	}
	s.mu.Lock()
	tool := s.tools[input.Name]
	allowed := s.candidate(input.Name)
	s.mu.Unlock()
	if !allowed {
		slog.Warn("tool request rejected", "tool", input.Name, "trigger", "call_tool")
		return nil, fmt.Errorf("tool %q is unavailable", input.Name)
	}
	if err := validateArguments(tool.InputSchema, input.Arguments); err != nil {
		return nil, fmt.Errorf("invalid arguments for %q: %w", input.Name, err)
	}
	if _, err := s.activate(input.Name, "call_tool"); err != nil {
		return nil, err
	}
	return s.forward(ctx, input.Name, input.Arguments, true, "call_tool")
}

func validateArguments(raw any, args map[string]any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(b, &schema); err != nil {
		return err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	return resolved.Validate(args)
}
