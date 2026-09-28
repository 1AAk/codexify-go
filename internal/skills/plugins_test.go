package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestCodexPluginDiscoveryUsesEnabledHighestVersionAndSkillRules(t *testing.T) {
	home := t.TempDir()
	v1 := filepath.Join(home, "plugins", "cache", "market", "sample", "1.0.0")
	v2 := filepath.Join(home, "plugins", "cache", "market", "sample", "2.0.0")
	writeLegacyPluginFixture(t, v1, "acme", "old", "Old skill")
	writeLegacyPluginFixture(t, v2, "acme", "new", "New skill")
	explicit := filepath.Join(v2, "skills", "new", "agents")
	if err := os.MkdirAll(explicit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(explicit, "openai.yaml"), []byte("policy:\n  allow_implicit_invocation: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := []byte("[plugins.\"sample@market\"]\nenabled = true\n")
	found, warnings := discoverCodexPluginSkillsFrom(home, cfg)
	if len(warnings) != 0 {
		t.Fatalf("warnings=%#v", warnings)
	}
	if len(found) != 1 || found[0].Name != "acme:new" {
		t.Fatalf("found=%#v", found)
	}
	if found[0].AllowImplicitInvocation {
		t.Fatal("openai.yaml explicit-only policy was ignored")
	}
	if strings.Contains(found[0].Path, home) || filepath.IsAbs(found[0].Path) {
		t.Fatalf("plugin skill path leaked host path: %q", found[0].Path)
	}

	cfg = []byte("[plugins.\"sample@market\"]\nenabled = true\n\n[[skills.config]]\nname = \"acme:new\"\nenabled = false\n")
	found, warnings = discoverCodexPluginSkillsFrom(home, cfg)
	if len(warnings) != 0 || len(found) != 0 {
		t.Fatalf("rule did not disable plugin skill: found=%#v warnings=%#v", found, warnings)
	}
}

func TestCodexPluginLocalVersionWinsAndFeatureCanDisable(t *testing.T) {
	home := t.TempDir()
	writeLegacyPluginFixture(t, filepath.Join(home, "plugins", "cache", "market", "sample", "9.0.0"), "sample", "release", "Release")
	writeLegacyPluginFixture(t, filepath.Join(home, "plugins", "cache", "market", "sample", "local"), "sample", "local", "Local")
	cfg := []byte("[plugins.\"sample@market\"]\nenabled = true\n")
	found, _ := discoverCodexPluginSkillsFrom(home, cfg)
	if len(found) != 1 || found[0].Name != "sample:local" {
		t.Fatalf("local version did not win: %#v", found)
	}

	disabled := []byte("[features]\nplugins = false\n[plugins.\"sample@market\"]\nenabled = true\n")
	found, _ = discoverCodexPluginSkillsFrom(home, disabled)
	if len(found) != 0 {
		t.Fatalf("plugins feature=false was ignored: %#v", found)
	}
}

func TestClaudePluginRegistryChoosesApplicableScopedInstallation(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "project")
	workDir := filepath.Join(project, "service")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userInstall := filepath.Join(home, "claude-user")
	localInstall := filepath.Join(home, "claude-local")
	writeClaudeSkillFixture(t, userInstall, "user-skill", "User skill")
	writeClaudeSkillFixture(t, localInstall, "local-skill", "Local skill")

	registry := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"sample@market": []any{
				map[string]any{"scope": "user", "installPath": userInstall},
				map[string]any{"scope": "local", "projectPath": project, "installPath": localInstall},
			},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	found, warnings := discoverClaudePluginSkillsFrom(home, workDir, data)
	if len(warnings) != 0 {
		t.Fatalf("warnings=%#v", warnings)
	}
	if len(found) != 1 || found[0].Name != "sample:local-skill" {
		t.Fatalf("wrong applicable Claude installation: %#v", found)
	}

	other := filepath.Join(home, "other-project")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	found, warnings = discoverClaudePluginSkillsFrom(home, other, data)
	if len(warnings) != 0 {
		t.Fatalf("warnings=%#v", warnings)
	}
	if len(found) != 1 || found[0].Name != "sample:user-skill" {
		t.Fatalf("project-scoped Claude install leaked into other project: %#v", found)
	}
}

func TestPluginsDefaultOffWhenExplicitSkillDirsOverrideUnlessForced(t *testing.T) {
	reader := New(config.SkillsConfig{Enabled: true, Dirs: []string{t.TempDir()}})
	if reader.pluginsEnabled() {
		t.Fatal("explicit skill dirs should suppress plugin scan by default")
	}
	yes := true
	reader = New(config.SkillsConfig{Enabled: true, Dirs: []string{t.TempDir()}, IncludePlugins: &yes})
	if !reader.pluginsEnabled() {
		t.Fatal("includePlugins=true should force plugin scan")
	}
	no := false
	reader = New(config.SkillsConfig{Enabled: true, IncludePlugins: &no})
	if reader.pluginsEnabled() {
		t.Fatal("includePlugins=false should disable plugin scan")
	}
}

func writeLegacyPluginFixture(t *testing.T, root, namespace, skillName, description string) {
	t.Helper()
	manifestDir := filepath.Join(root, ".codex-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "{\"name\":\"" + namespace + "\"}"
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSkillFixture(t, filepath.Join(root, "skills", skillName), skillName, description)
}

func writeClaudeSkillFixture(t *testing.T, install, skillName, description string) {
	t.Helper()
	writeSkillFixture(t, filepath.Join(install, "skills", skillName), skillName, description)
}

func writeSkillFixture(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
