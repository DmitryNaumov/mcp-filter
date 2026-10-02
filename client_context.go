package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type clientContextKey struct{}
type connectionContextKey struct{}

var connectionSequence atomic.Uint64
var requestSequence atomic.Uint64

// Only these explicit correlation fields are inspected. All values remain
// client assertions, and are hashed even if they look like ordinary UUIDs.
type reportedID struct{ value, source string }
type reportedContext struct {
	ids             map[string]reportedID
	metadataPresent bool
	metadataStatus  string
	fieldCount      int
}

func readReportedContext(meta map[string]any, header string, source string) reportedContext {
	c := reportedContext{ids: make(map[string]reportedID), metadataStatus: "absent", fieldCount: len(meta)}
	if v, ok := meta["openai/session"].(string); ok && validCorrelationValue(v) {
		c.ids["session"] = reportedID{v, source + ".openai/session"}
	}
	raw, present := meta["x-codex-turn-metadata"]
	if !present && header != "" {
		raw, present = header, true
		source = source + "_header"
	}
	c.metadataPresent = present
	if !present {
		return c
	}
	c.metadataStatus = "invalid"
	var object map[string]any
	switch value := raw.(type) {
	case map[string]any:
		object = value
		c.metadataStatus = "object"
	case string:
		// Bound processing of unknown/untrusted metadata, not just log size.
		if len(value) <= 8192 && json.Unmarshal([]byte(value), &object) == nil && object != nil {
			c.metadataStatus = "json_object"
		}
	}
	for _, field := range []struct{ wire, kind string }{{"threadId", "thread"}, {"turnId", "turn"}, {"callId", "call"}} {
		if v, ok := object[field.wire].(string); ok && validCorrelationValue(v) {
			c.ids[field.kind] = reportedID{v, source + ".x-codex-turn-metadata"}
		}
	}
	return c
}

func validCorrelationValue(v string) bool {
	return len(v) > 0 && len(v) <= 512 && strings.TrimSpace(v) == v
}

func clientIDFingerprint(kind, value string) string {
	return messageFingerprint("client-id\x00" + kind + "\x00" + value)
}

func (c reportedContext) fields() []any {
	fields := []any{"client_meta_field_count", c.fieldCount, "codex_turn_metadata_present", c.metadataPresent, "codex_turn_metadata_status", c.metadataStatus}
	for _, kind := range []string{"session", "thread", "turn", "call"} {
		if id, ok := c.ids[kind]; ok {
			fields = append(fields, "client_"+kind+"_id_fingerprint", clientIDFingerprint(kind, id.value), "client_"+kind+"_id_source", id.source)
		}
	}
	return fields
}

func clientContextFields(ctx context.Context) []any {
	fields, _ := ctx.Value(clientContextKey{}).([]any)
	return fields
}

var numericVersion = regexp.MustCompile(`^v?[0-9]{1,6}(\.[0-9]{1,6}){1,3}$`)
var protocolDate = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func initializeClientFields(params *mcp.InitializeParams) []any {
	fields := []any{}
	if params == nil {
		return fields
	}
	if protocolDate.MatchString(params.ProtocolVersion) {
		fields = append(fields, "client_protocol_version", params.ProtocolVersion)
	}
	if info := params.ClientInfo; info != nil {
		family := "unknown"
		switch info.Name {
		case "codex", "codex_cli_rs", "codex-desktop":
			family = "codex"
		}
		fields = append(fields, "client_family", family, "client_name_fingerprint", messageFingerprint(info.Name), "client_version_fingerprint", messageFingerprint(info.Version))
		if numericVersion.MatchString(info.Version) {
			fields = append(fields, "client_version", info.Version)
		}
	}
	return fields
}

func clientContextMiddleware(entry string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "initialize" && method != "tools/call" {
				return next(ctx, method, req)
			}
			connectionID, _ := ctx.Value(connectionContextKey{}).(string)
			fields := []any{"entry", entry, "client_connection_id", connectionID, "client_connection_scope", "filter_local"}
			header := ""
			if extra := req.GetExtra(); extra != nil {
				header = extra.Header.Get("x-codex-turn-metadata")
			}
			reported := readReportedContext(req.GetParams().GetMeta(), header, "request_meta")
			if method == "tools/call" {
				// Initialize is a connection-level fallback only. Never carry a turn or
				// call ID from initialize (or from another, possibly concurrent call).
				if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
					if params := ss.InitializeParams(); params != nil {
						initial := readReportedContext(params.GetMeta(), "", "initialize_meta")
						for _, kind := range []string{"session", "thread"} {
							if _, exists := reported.ids[kind]; !exists {
								if id, ok := initial.ids[kind]; ok {
									reported.ids[kind] = id
								}
							}
						}
					}
				}
				fields = append(fields, "client_request_id", runID+":request:"+strconv.FormatUint(requestSequence.Add(1), 10))
				if call, ok := req.(*mcp.CallToolRequest); ok {
					fields = append(fields, "client_tool", call.Params.Name)
				}
			}
			fields = append(fields, reported.fields()...)
			ctx = context.WithValue(ctx, clientContextKey{}, fields)
			if method == "initialize" {
				result, err := next(ctx, method, req)
				if params, ok := req.GetParams().(*mcp.InitializeParams); ok {
					fields = append(fields, initializeClientFields(params)...)
				}
				fields = append(fields, "event", "client_initialize_context", "initialize_success", err == nil)
				slog.InfoContext(ctx, "client initialization context", fields...)
				return result, err
			}
			slog.InfoContext(ctx, "client call context", append(fields, "event", "client_call_context")...)
			return next(ctx, method, req)
		}
	}
}

func launcherContextFields() []any {
	fields := []any{"ppid", os.Getppid()}
	if value := os.Getenv("CODEX_THREAD_ID"); validCorrelationValue(value) {
		fields = append(fields, "launcher_thread_id_fingerprint", clientIDFingerprint("thread", value), "launcher_thread_id_source", "env.CODEX_THREAD_ID")
	}
	return fields
}
