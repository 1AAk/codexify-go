package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestListAndReadSkill(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, ".agents", "skills", "demo")
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: demo\ndescription: Use this for demo tasks\n---\n\n# Demo\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reference.txt"), []byte("reference body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "openai.yaml"), []byte("policy:\n  allow_implicit_invocation: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	includePlugins := false
	reader := New(config.SkillsConfig{Enabled: true, IncludeUser: false, IncludePlugins: &includePlugins})
	catalog, err := reader.List(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) != 1 {
		t.Fatalf("skills = %#v", catalog.Skills)
	}
	skill := catalog.Skills[0]
	if skill.Name != "demo" || skill.AllowImplicitInvocation {
		t.Fatalf("skill = %#v", skill)
	}
	if filepath.IsAbs(skill.Path) || strings.Contains(skill.Path, project) {
		t.Fatalf("skill path leaked host path: %q", skill.Path)
	}

	read, err := reader.Read(project, ReadInput{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Content, "Do the thing") || !strings.Contains(read.Content, "reference.txt") {
		t.Fatalf("read = %q", read.Content)
	}
	if strings.Contains(read.Content, project) {
		t.Fatalf("skills_read leaked host path: %q", read.Content)
	}
	resource, err := reader.Read(project, ReadInput{Name: "demo", Resource: "reference.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resource.Content, "reference body") {
		t.Fatalf("resource = %q", resource.Content)
	}
	if _, err := reader.Read(project, ReadInput{Name: "demo", Resource: "../outside.txt"}); err == nil {
		t.Fatal("expected resource traversal to be rejected")
	}
}

func TestFrontmatterRequiresDescription(t *testing.T) {
	if _, _, err := parseFrontmatter([]byte("---\nname: bad\n---\n"), "bad"); err == nil {
		t.Fatal("expected missing description to fail")
	}
}
