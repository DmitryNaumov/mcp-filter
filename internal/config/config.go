package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	BaseFile  = ".mcp-filter.json"
	LocalFile = ".mcp-filter.local.json"
)

type Config struct {
	Entries map[string]Entry `json:"entries"`
}

type Entry struct {
	Allow    []string `json:"allow"`
	Metadata Metadata `json:"metadata"`
}

type Metadata struct {
	Server  map[string]any            `json:"server"`
	Tools   map[string]map[string]any `json:"tools"`
	Patches []Patch                   `json:"patches"`
}

type Patch struct {
	Method string         `json:"method"`
	Select map[string]any `json:"select"`
	Patch  map[string]any `json:"patch"`
}

func Load(dir string) (Config, error) {
	basePath, err := ResolvePath(dir)
	if err != nil {
		return Config{}, err
	}
	return LoadPath(basePath)
}

// ResolvePath finds the base rules file by walking from dir towards the root.
func ResolvePath(dir string) (string, error) {
	return findUp(dir, BaseFile)
}

// LoadPath loads an explicit base rules file and an optional local override
// beside it. It is useful when the MCP client starts the proxy outside a repo.
func LoadPath(basePath string) (Config, error) {
	base, err := read(basePath)
	if err != nil {
		return Config{}, err
	}
	local, err := readOptional(filepath.Join(filepath.Dir(basePath), LocalFile))
	if err != nil {
		return Config{}, err
	}
	return merge(base, local), nil
}

func findUp(dir, name string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found from %s upward", name, dir)
		}
		dir = parent
	}
}

func read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Entries == nil {
		cfg.Entries = map[string]Entry{}
	}
	return cfg, nil
}

func readOptional(path string) (Config, error) {
	cfg, err := read(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	return cfg, err
}

func merge(base, local Config) Config {
	result := Config{Entries: make(map[string]Entry, len(base.Entries)+len(local.Entries))}
	for name, entry := range base.Entries {
		result.Entries[name] = entry
	}
	for name, override := range local.Entries {
		current := result.Entries[name]
		if override.Allow != nil {
			current.Allow = override.Allow
		}
		current.Metadata.Server = MergePatch(current.Metadata.Server, override.Metadata.Server)
		current.Metadata.Tools = mergeTools(current.Metadata.Tools, override.Metadata.Tools)
		if override.Metadata.Patches != nil {
			current.Metadata.Patches = override.Metadata.Patches
		}
		result.Entries[name] = current
	}
	return result
}

func mergeTools(base, local map[string]map[string]any) map[string]map[string]any {
	if base == nil && local == nil {
		return nil
	}
	result := make(map[string]map[string]any, len(base)+len(local))
	for name, patch := range base {
		result[name] = MergePatch(nil, patch)
	}
	for name, patch := range local {
		result[name] = MergePatch(result[name], patch)
	}
	return result
}

// MergePatch applies RFC 7396 semantics to JSON object values.
func MergePatch(target, patch map[string]any) map[string]any {
	result := make(map[string]any, len(target)+len(patch))
	for key, value := range target {
		result[key] = value
	}
	for key, value := range patch {
		if value == nil {
			delete(result, key)
			continue
		}
		patchObject, patchIsObject := value.(map[string]any)
		targetObject, targetIsObject := result[key].(map[string]any)
		if patchIsObject {
			if !targetIsObject {
				targetObject = nil
			}
			result[key] = MergePatch(targetObject, patchObject)
			continue
		}
		result[key] = value
	}
	return result
}

func (c Config) Entry(name string) (Entry, error) {
	entry, ok := c.Lookup(name)
	if !ok {
		return Entry{}, fmt.Errorf("entry %q not configured", name)
	}
	return entry, nil
}

// Lookup returns an entry only when it was explicitly present in either rules
// file. An absent entry is deliberately different from a present entry with no
// allowlist: callers use the former for transparent pass-through and reject the
// latter as an unsafe, incomplete filter rule.
func (c Config) Lookup(name string) (Entry, bool) {
	entry, ok := c.Entries[name]
	return entry, ok
}

func (e Entry) Allowed(name string) bool {
	for _, allowed := range e.Allow {
		if allowed == name {
			return true
		}
	}
	return false
}
