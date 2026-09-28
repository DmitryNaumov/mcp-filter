package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionDetailsUsesEmbeddedVCSMetadata(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20200101000000-aaaaaaaaaaaa"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-09-28T11:19:49Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := formatVersionDetails(info)
	for _, want := range []string{
		"Commit: 0123456789abcdef0123456789abcdef01234567",
		"Commit date: 2026-09-28T11:19:49Z",
		"Source modified: true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestVersionDetailsUsesPseudoVersionWhenVCSMetadataMissing(t *testing.T) {
	for _, moduleVersion := range []string{
		"v0.0.0-20260928111949-bcab20384cde",
		"v1.2.4-0.20260928111949-bcab20384cde",
		"v1.2.3-pre.0.20260928111949-bcab20384cde",
		"v2.0.1-20260928111949-bcab20384cde+incompatible",
	} {
		t.Run(moduleVersion, func(t *testing.T) {
			got := formatVersionDetails(&debug.BuildInfo{Main: debug.Module{Version: moduleVersion}})
			if !strings.Contains(got, "Commit: bcab20384cde") || !strings.Contains(got, "Commit date: 2026-09-28T11:19:49Z") {
				t.Fatalf("pseudo-version metadata missing: %q", got)
			}
		})
	}
}

func TestVersionDetailsDoesNotInventCommitForTaggedOrUnknownBuild(t *testing.T) {
	for _, info := range []*debug.BuildInfo{nil, {Main: debug.Module{Version: "v1.2.3"}}} {
		got := formatVersionDetails(info)
		if !strings.Contains(got, "Commit: unavailable") || !strings.Contains(got, "Commit date: unavailable") {
			t.Fatalf("unexpected commit details: %q", got)
		}
	}
}
