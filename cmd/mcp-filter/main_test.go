package main

import (
	"testing"

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
