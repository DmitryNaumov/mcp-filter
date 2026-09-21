package config

import "testing"

func TestMergeLocalReplacesAllowButRetainsToolMetadata(t *testing.T) {
	base := Config{Entries: map[string]Entry{"tracker": {
		Allow:    []string{"get", "search"},
		Metadata: Metadata{Tools: map[string]map[string]any{"search": {"description": "search issues"}}},
	}}}
	local := Config{Entries: map[string]Entry{"tracker": {Allow: []string{"get"}}}}
	got := merge(base, local).Entries["tracker"]
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
