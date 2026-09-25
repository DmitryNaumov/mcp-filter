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

func TestReadRejectsModeField(t *testing.T) {
	path := filepath.Join(t.TempDir(), BaseFile)
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"tracker":{"mode":"strict","allow":["get"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := read(path)
	if err == nil || !strings.Contains(err.Error(), "remove mode") {
		t.Fatalf("expected mode migration error, got %v", err)
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

func TestMergeLocalOverridesOnlySpecifiedLoggingFields(t *testing.T) {
	baseDirectory, baseLevel, localLevel := "logs", "warn", "debug"
	base := Config{Logging: &Logging{Directory: &baseDirectory, Level: &baseLevel}}
	local := Config{Logging: &Logging{Level: &localLevel}}
	got := merge(base, local).Logging
	if got == nil || got.Directory == nil || *got.Directory != "logs" || got.Level == nil || *got.Level != "debug" {
		t.Fatalf("unexpected merged logging settings: %#v", got)
	}
}

func TestLoadPathRejectsIncompleteLoggingConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), BaseFile)
	if err := os.WriteFile(path, []byte(`{"logging":{"level":"debug"},"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPath(path); err == nil || !strings.Contains(err.Error(), "logging.directory") {
		t.Fatalf("expected logging directory error, got %v", err)
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

func TestEntryAllowsMissingAllowAndRejectsOverlap(t *testing.T) {
	if err := (Entry{Deny: []string{"secret"}}).Validate(); err != nil {
		t.Fatalf("entry without allow rejected: %v", err)
	}
	if err := (Entry{Allow: []string{"secret"}, Deny: []string{"secret"}}).Validate(); err == nil {
		t.Fatal("overlapping allow and deny accepted")
	}
}

func TestMergeLocalOverridesDeny(t *testing.T) {
	base := Config{MCPServers: map[string]Entry{"tracker": {Allow: []string{"a"}, Deny: []string{"c"}}}}
	local := Config{MCPServers: map[string]Entry{"tracker": {Deny: []string{"b"}}}}
	entry := merge(base, local).MCPServers["tracker"]
	if !entry.Denied("b") || entry.Denied("c") || !entry.Allowed("a") {
		t.Fatalf("bad merge: %#v", entry)
	}
}
