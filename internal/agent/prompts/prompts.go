// Package prompts holds the agent stage prompt templates as embedded Go
// text/template files (one .md per stage plus shared snippets and a vendored copy
// of audiosilo-meta's AUTHORING.md). Templates render with missingkey=error so a
// stage that forgets a field fails loudly at render time rather than emitting a
// prompt with an empty hole. House rule: the prompt text uses hyphens, never em
// dashes, so the agent it drives keeps to the same style.
package prompts

import (
	"embed"
	"strings"
	"text/template"
)

//go:embed *.md
var files embed.FS

// Render parses the embedded template named name (e.g. "factpass.md") and executes
// it against data. missingkey=error means referencing a field absent from data is
// an error, not a silent blank. A template with no actions renders regardless of
// data (pass nil).
func Render(name string, data any) (string, error) {
	b, err := files.ReadFile(name)
	if err != nil {
		return "", err
	}
	return execTemplate(name, string(b), data)
}

// execTemplate is the single render code path shared by Render and its tests, so a
// literal-string template exercises exactly the same parser/executor/options as an
// embedded file.
func execTemplate(name, text string, data any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}
