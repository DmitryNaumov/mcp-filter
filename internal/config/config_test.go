package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRejectsRetiredEntriesField(t *testing.T) {
	path := filepath.Join(t.TempDir(), BaseFile)
	if err := os.WriteFile(path, []byte(`{"entries":{"tracker":{"allow":["get"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := read(path)
	if err == nil || !strings.Contains(err.Error(), "mcpServers") {
		t.Fatalf("expected mcpServers migration error, got %v", err)
	}
}

func TestMergeLocalReplacesAllowButRetainsToolMetadata(t *testing.T) {
	base := Config{MCPServers: map[string]Entry{"tracker": {
		Allow:    []string{"get", "search"},
		Metadata: Metadata{Tools: map[string]map[string]any{"search": {"description": "search issues"}}},
	}}}
	local := Config{MCPServers: map[string]Entry{"tracker": {Allow: []string{"get"}}}}
	got := merge(base, local).MCPServers["tracker"]
	if got.Allowed("search") || !got.Allowed("get") {
		t.Fatalf("allowlist was not replaced: %#v", got.Allow)
	}
	if got.Metadata.Tools["search"]["description"] != "search issues" {
		t.Fatalf("tool metadata was lost: %#v", got.Metadata.Tools)
	}
}

func TestMergePatchDeletesAndMerges(t *testing.T) {
	got := MergePatch(map[string]any{"title": "old", "annotations": map[string]any{"readOnlyHint": true}}, map[string]any{"title": nil, "annotations": map[string]any{"destructiveHint": false}})
	if _, ok := got["title"]; ok {
		t.Fatal("title was not deleted")
	}
	annotations := got["annotations"].(map[string]any)
	if annotations["readOnlyHint"] != true || annotations["destructiveHint"] != false {
		t.Fatalf("nested merge failed: %#v", annotations)
	}
}
