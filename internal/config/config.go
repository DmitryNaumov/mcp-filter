package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	BaseFile  = ".mcp-filter.json"
	LocalFile = ".mcp-filter.local.json"
)

type Config struct {
	Schema     string           `json:"$schema"`
	Logging    *Logging         `json:"logging"`
	MCPServers map[string]Entry `json:"mcpServers"`
}

// Logging configures diagnostics produced by mcp-filter itself. It deliberately
// contains no request or response logging options: tool arguments, results,
// headers and other secret-bearing values are never logged.
//
// Pointers preserve the distinction between an omitted local override and an
// explicitly supplied value while base and local configurations are merged.
type Logging struct {
	Directory *string `json:"directory"`
	Level     *string `json:"level"`
	Format    *string `json:"format"`
}

type Entry struct {
	Allow    []string `json:"allow"`
	Deny     []string `json:"deny"`
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
	result := merge(base, local)
	if err := result.ValidateLogging(); err != nil {
		return Config{}, err
	}
	for name, entry := range result.MCPServers {
		if err := entry.Validate(); err != nil {
			return Config{}, fmt.Errorf("entry %q: %w", name, err)
		}
	}
	return result, nil
}

// ValidateLogging validates the effective (base plus local) logging settings.
// It is intentionally performed after merging, so a local file may override
// only the log level or format while inheriting the base directory.
func (c Config) ValidateLogging() error {
	if c.Logging == nil {
		return nil
	}
	if c.Logging.Directory == nil || strings.TrimSpace(*c.Logging.Directory) == "" {
		return errors.New("logging.directory is required when logging is configured")
	}
	if c.Logging.Level != nil {
		switch *c.Logging.Level {
		case "", "error", "warn", "info", "debug":
		default:
			return fmt.Errorf("unsupported logging.level %q", *c.Logging.Level)
		}
	}
	if c.Logging.Format != nil {
		switch *c.Logging.Format {
		case "", "text", "json":
		default:
			return fmt.Errorf("unsupported logging.format %q", *c.Logging.Format)
		}
	}
	return nil
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, legacy := fields["entries"]; legacy {
		return Config{}, fmt.Errorf("parse %s: use %q instead of the retired %q field", path, "mcpServers", "entries")
	}
	if raw, ok := fields["mcpServers"]; ok {
		var servers map[string]map[string]json.RawMessage
		if err := json.Unmarshal(raw, &servers); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
		for name, entry := range servers {
			if _, hasMode := entry["mode"]; hasMode {
				return Config{}, fmt.Errorf("parse %s: entry %q: remove mode; tools outside allow and deny are automatically discoverable", path, name)
			}
		}
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]Entry{}
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
	result := Config{Logging: mergeLogging(base.Logging, local.Logging), MCPServers: make(map[string]Entry, len(base.MCPServers)+len(local.MCPServers))}
	for name, entry := range base.MCPServers {
		result.MCPServers[name] = entry
	}
	for name, override := range local.MCPServers {
		current := result.MCPServers[name]
		if override.Allow != nil {
			current.Allow = override.Allow
		}
		if override.Deny != nil {
			current.Deny = override.Deny
		}
		current.Metadata.Server = MergePatch(current.Metadata.Server, override.Metadata.Server)
		current.Metadata.Tools = mergeTools(current.Metadata.Tools, override.Metadata.Tools)
		if override.Metadata.Patches != nil {
			current.Metadata.Patches = override.Metadata.Patches
		}
		result.MCPServers[name] = current
	}
	return result
}

func mergeLogging(base, local *Logging) *Logging {
	if base == nil && local == nil {
		return nil
	}
	result := &Logging{}
	if base != nil {
		*result = *base
	}
	if local != nil {
		if local.Directory != nil {
			result.Directory = local.Directory
		}
		if local.Level != nil {
			result.Level = local.Level
		}
		if local.Format != nil {
			result.Format = local.Format
		}
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
	entry, ok := c.MCPServers[name]
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

func (e Entry) Denied(name string) bool {
	for _, denied := range e.Deny {
		if denied == name {
			return true
		}
	}
	return false
}

func (e Entry) Validate() error {
	for _, name := range e.Allow {
		if e.Denied(name) {
			return fmt.Errorf("tool %q is in both allow and deny", name)
		}
	}
	return nil
}
