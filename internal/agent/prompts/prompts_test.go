package prompts

import (
	"strings"
	"testing"
)

func TestRenderEmbeddedAuthoring(t *testing.T) {
	// authoring.md has no template actions, so it renders regardless of data.
	out, err := Render("authoring.md", nil)
	if err != nil {
		t.Fatalf("Render authoring.md: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("authoring.md rendered empty")
	}
}

func TestRenderMissingFile(t *testing.T) {
	if _, err := Render("does-not-exist.md", nil); err == nil {
		t.Fatal("Render of a missing file should error")
	}
}

func TestExecTemplateSameCodePath(t *testing.T) {
	// A literal-string template runs through exactly the same parse/execute/options
	// path Render uses for embedded files.
	out, err := execTemplate("lit", "Hello {{.Name}}, book {{.Title}}", map[string]string{"Name": "El", "Title": "A Deadly Education"})
	if err != nil {
		t.Fatalf("execTemplate: %v", err)
	}
	if out != "Hello El, book A Deadly Education" {
		t.Errorf("rendered = %q", out)
	}
}

func TestExecTemplateMissingKeyIsError(t *testing.T) {
	// missingkey=error: referencing a field absent from data must fail loudly.
	if _, err := execTemplate("lit", "{{.Absent}}", map[string]string{}); err == nil {
		t.Fatal("missing key should be an error")
	}
}

func TestEmbedIncludesAuthoring(t *testing.T) {
	entries, err := files.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		if e.Name() == "authoring.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("embedded prompts missing authoring.md: %v", names)
	}
}
