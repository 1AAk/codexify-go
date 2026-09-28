package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/semver"
)

const (
	agentPluginSchemaURI = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	maxPluginScanDepth   = 6
	maxPluginScanEntries = 20000
)

type pluginSkillRule struct {
	Name    string
	Path    string
	Enabled bool
}

type codexConfig struct {
	Features struct {
		Plugins *bool `toml:"plugins"`
	} `toml:"features"`
	Plugins map[string]struct {
		Enabled *bool `toml:"enabled"`
	} `toml:"plugins"`
	Skills struct {
		Config []struct {
			Name    string `toml:"name"`
			Path    string `toml:"path"`
			Enabled *bool  `toml:"enabled"`
		} `toml:"config"`
	} `toml:"skills"`
}

type pluginManifest struct {
	Name   string
	Roots  []string
	Direct bool
}

type claudeRegistry struct {
	Version int                        `json:"version"`
	Plugins map[string]json.RawMessage `json:"plugins"`
}

type claudeInstall struct {
	Scope       string `json:"scope,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	InstallPath string `json:"installPath"`
}

func (r *Reader) discoverPluginSkills(workDir string) ([]Skill, []string) {
	if !r.pluginsEnabled() {
		return nil, nil
	}
	var out []Skill
	var warnings []string
	codex, codexWarnings := discoverCodexPluginSkills(workDir)
	out = append(out, codex...)
	warnings = append(warnings, codexWarnings...)
	if r.cfg.ClaudePlugins {
		claude, claudeWarnings := discoverClaudePluginSkills(workDir)
		out = append(out, claude...)
		warnings = append(warnings, claudeWarnings...)
	}
	return out, warnings
}

func (r *Reader) pluginsEnabled() bool {
	if r.cfg.IncludePlugins != nil {
		return *r.cfg.IncludePlugins
	}
	return len(r.cfg.Dirs) == 0
}

func discoverCodexPluginSkills(workDir string) ([]Skill, []string) {
	configPath, codexHome, err := codexConfigPath()
	if err != nil {
		return nil, []string{err.Error()}
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("cannot read Codex plugin configuration: %v", err)}
	}
	return discoverCodexPluginSkillsFrom(codexHome, data)
}

func discoverCodexPluginSkillsFrom(codexHome string, data []byte) ([]Skill, []string) {
	var cfg codexConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, []string{"cannot parse Codex plugin configuration"}
	}
	if cfg.Features.Plugins != nil && !*cfg.Features.Plugins {
		return nil, nil
	}
	rules := make([]pluginSkillRule, 0, len(cfg.Skills.Config))
	for _, raw := range cfg.Skills.Config {
		if raw.Enabled == nil {
			continue
		}
		rules = append(rules, pluginSkillRule{Name: strings.TrimSpace(raw.Name), Path: strings.TrimSpace(raw.Path), Enabled: *raw.Enabled})
	}

	keys := make([]string, 0, len(cfg.Plugins))
	for key := range cfg.Plugins {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var skills []Skill
	var warnings []string
	for _, key := range keys {
		entry := cfg.Plugins[key]
		if entry.Enabled != nil && !*entry.Enabled {
			continue
		}
		plugin, market, ok := strings.Cut(key, "@")
		if !ok || !validPluginSegment(plugin, true) || !validPluginSegment(market, false) {
			continue
		}
		base := filepath.Join(codexHome, "plugins", "cache", market, plugin)
		version := activePluginVersion(base)
		if version == "" {
			continue
		}
		root := filepath.Join(base, version)
		manifest, err := loadPluginManifest(root)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Codex plugin %q: %v", key, err))
			continue
		}
		for _, skillRoot := range manifest.Roots {
			found, foundWarnings := discoverPluginRoot(skillRoot, manifest.Name, manifest.Direct)
			for _, skill := range found {
				if pluginSkillEnabled(skill, rules) {
					skills = append(skills, skill)
				}
			}
			warnings = append(warnings, foundWarnings...)
		}
	}
	return skills, warnings
}

func codexConfigPath() (string, string, error) {
	if explicit := strings.TrimSpace(os.Getenv("CODEX_HOME")); explicit != "" {
		info, err := os.Stat(explicit)
		if err != nil || !info.IsDir() {
			return "", "", errors.New("CODEX_HOME does not point to a readable directory")
		}
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(abs, "config.toml"), abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", errors.New("cannot resolve user home for Codex plugin discovery")
	}
	codexHome := filepath.Join(home, ".codex")
	return filepath.Join(codexHome, "config.toml"), codexHome, nil
}

func activePluginVersion(base string) string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	var versions []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "local" {
			return "local"
		}
		if validPluginVersion(name) {
			versions = append(versions, name)
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		a, b := versions[i], versions[j]
		va, vb := a, b
		if !strings.HasPrefix(va, "v") {
			va = "v" + va
		}
		if !strings.HasPrefix(vb, "v") {
			vb = "v" + vb
		}
		if semver.IsValid(va) && semver.IsValid(vb) {
			return semver.Compare(va, vb) < 0
		}
		return a < b
	})
	if len(versions) == 0 {
		return ""
	}
	return versions[len(versions)-1]
}

func loadPluginManifest(root string) (pluginManifest, error) {
	agentPath := filepath.Join(root, "plugin.json")
	if data, err := os.ReadFile(agentPath); err == nil {
		var raw struct {
			Schema string `json:"$schema"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(data, &raw) == nil && raw.Schema != "" {
			if raw.Schema != agentPluginSchemaURI {
				return pluginManifest{}, fmt.Errorf("unsupported Agent Plugin schema %q", raw.Schema)
			}
			if !validPluginSegment(raw.Name, true) {
				return pluginManifest{}, fmt.Errorf("invalid Agent Plugin name %q", raw.Name)
			}
			return pluginManifest{Name: raw.Name, Roots: []string{filepath.Join(root, "skills")}, Direct: true}, nil
		}
	}

	for _, rel := range []string{filepath.Join(".codex-plugin", "plugin.json"), filepath.Join(".claude-plugin", "plugin.json"), filepath.Join(".cursor-plugin", "plugin.json")} {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var raw struct {
			Name   string          `json:"name"`
			Skills json.RawMessage `json:"skills"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return pluginManifest{}, fmt.Errorf("invalid plugin manifest")
		}
		namespace := strings.TrimSpace(raw.Name)
		if namespace == "" {
			namespace = filepath.Base(root)
		}
		var roots []string
		if len(raw.Skills) > 0 && string(raw.Skills) != "null" {
			var one string
			if json.Unmarshal(raw.Skills, &one) == nil {
				if resolved := pluginRelative(root, one); resolved != "" {
					roots = append(roots, resolved)
				}
			} else {
				var many []string
				if json.Unmarshal(raw.Skills, &many) == nil {
					for _, item := range many {
						if resolved := pluginRelative(root, item); resolved != "" {
							roots = append(roots, resolved)
						}
					}
				}
			}
		}
		if len(roots) == 0 {
			if info, err := os.Stat(filepath.Join(root, "skills")); err == nil && info.IsDir() {
				roots = append(roots, filepath.Join(root, "skills"))
			}
		}
		if info, err := os.Stat(filepath.Join(root, ".codex-plugin", "migrated-command-skills")); err == nil && info.IsDir() {
			roots = append(roots, filepath.Join(root, ".codex-plugin", "migrated-command-skills"))
		}
		if len(roots) == 0 {
			return pluginManifest{}, errors.New("plugin manifest has no usable skill roots")
		}
		return pluginManifest{Name: namespace, Roots: roots}, nil
	}
	return pluginManifest{}, errors.New("missing plugin manifest")
}

func pluginRelative(root, raw string) string {
	raw = filepath.ToSlash(strings.TrimSpace(raw))
	if !strings.HasPrefix(raw, "./") {
		return ""
	}
	rel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(raw, "./")))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.Join(root, rel)
}

func discoverPluginRoot(root, namespace string, direct bool) ([]Skill, []string) {
	var skills []Skill
	var warnings []string
	entries := 0
	walk := func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if entry.IsDir() {
			if direct && depth > 1 {
				return filepath.SkipDir
			}
			if !direct && depth > maxPluginScanDepth {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxPluginScanEntries {
			return errors.New("plugin scan entry limit reached")
		}
		if entry.Name() != FileName {
			return nil
		}
		dir := filepath.Dir(path)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		name, description, parseErr := parseFrontmatter(data, filepath.Base(dir))
		if parseErr != nil {
			warnings = append(warnings, fmt.Sprintf("plugin skill %s: %v", filepath.ToSlash(rel), parseErr))
			return nil
		}
		qualified := namespace + ":" + name
		if len(qualified) > maxSkillNameBytes*2+1 {
			warnings = append(warnings, fmt.Sprintf("plugin skill %s has an overlong qualified name", filepath.ToSlash(rel)))
			return nil
		}
		skills = append(skills, Skill{
			Name:                    qualified,
			Description:             description,
			AllowImplicitInvocation: implicitAllowed(dir),
			Scope:                   "plugin",
			Path:                    "plugin:" + filepath.ToSlash(filepath.Join(namespace, name, FileName)),
			dir:                     dir,
		})
		return nil
	}
	if direct {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, nil
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, entry.Name(), FileName)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			_ = walk(path, dirEntryFile(path), nil)
		}
	} else {
		_ = filepath.WalkDir(root, walk)
	}
	return skills, warnings
}

type simpleFileEntry string

func (e simpleFileEntry) Name() string               { return filepath.Base(string(e)) }
func (e simpleFileEntry) IsDir() bool                { return false }
func (e simpleFileEntry) Type() os.FileMode          { return 0 }
func (e simpleFileEntry) Info() (os.FileInfo, error) { return os.Stat(string(e)) }
func dirEntryFile(path string) os.DirEntry           { return simpleFileEntry(path) }

func pluginSkillEnabled(skill Skill, rules []pluginSkillRule) bool {
	enabled := true
	canonical, _ := filepath.Abs(filepath.Join(skill.dir, FileName))
	for _, rule := range rules {
		match := false
		if rule.Name != "" && rule.Name == skill.Name {
			match = true
		}
		if rule.Path != "" && filepath.IsAbs(rule.Path) {
			if target, err := filepath.Abs(rule.Path); err == nil && strings.EqualFold(filepath.Clean(target), filepath.Clean(canonical)) {
				match = true
			}
		}
		if match {
			enabled = rule.Enabled
		}
	}
	return enabled
}

func discoverClaudePluginSkills(workDir string) ([]Skill, []string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, nil
	}
	registryPath := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	data, err := os.ReadFile(registryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{"cannot read Claude plugin registry"}
	}
	return discoverClaudePluginSkillsFrom(home, workDir, data)
}

func discoverClaudePluginSkillsFrom(home, workDir string, data []byte) ([]Skill, []string) {
	var registry claudeRegistry
	if err := json.Unmarshal(data, &registry); err != nil || registry.Plugins == nil {
		return nil, []string{"cannot parse Claude plugin registry"}
	}
	workAbs, _ := filepath.Abs(workDir)
	var skills []Skill
	var warnings []string
	keys := make([]string, 0, len(registry.Plugins))
	for key := range registry.Plugins {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		plugin, market, ok := strings.Cut(key, "@")
		if !ok || plugin == "" || market == "" {
			continue
		}
		var installs []claudeInstall
		raw := registry.Plugins[key]
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '[' {
			if json.Unmarshal(raw, &installs) != nil {
				warnings = append(warnings, fmt.Sprintf("invalid Claude plugin %q installation", key))
				continue
			}
		} else {
			var one claudeInstall
			if json.Unmarshal(raw, &one) != nil {
				warnings = append(warnings, fmt.Sprintf("invalid Claude plugin %q installation", key))
				continue
			}
			installs = []claudeInstall{one}
		}
		best := -1
		bestDepth := -1
		var chosen claudeInstall
		for _, install := range installs {
			priority, depth, ok := claudeInstallPriority(install, workAbs)
			if !ok {
				continue
			}
			if priority > best || (priority == best && depth > bestDepth) {
				best, bestDepth, chosen = priority, depth, install
			}
		}
		if best < 0 || !filepath.IsAbs(chosen.InstallPath) {
			continue
		}
		root := filepath.Join(chosen.InstallPath, "skills")
		found, foundWarnings := discoverPluginRoot(root, plugin, true)
		skills = append(skills, found...)
		warnings = append(warnings, foundWarnings...)
	}
	return skills, warnings
}

func claudeInstallPriority(install claudeInstall, workDir string) (int, int, bool) {
	scope := strings.TrimSpace(install.Scope)
	if scope == "" {
		scope = "user"
	}
	switch scope {
	case "user":
		return 0, 0, true
	case "managed":
		return 3, 0, true
	case "project", "local":
		if !filepath.IsAbs(install.ProjectPath) {
			return 0, 0, false
		}
		project, _ := filepath.Abs(install.ProjectPath)
		rel, err := filepath.Rel(project, workDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return 0, 0, false
		}
		priority := 1
		if scope == "local" {
			priority = 2
		}
		return priority, len(strings.Split(filepath.Clean(project), string(filepath.Separator))), true
	default:
		return 0, 0, false
	}
}

func validPluginSegment(value string, dots bool) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	if dots && (strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..")) {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || (dots && r == '.') {
			continue
		}
		return false
	}
	return true
}

func validPluginVersion(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.+", r) {
			continue
		}
		return false
	}
	return true
}
