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

// auditJSONPrompts are every prompt whose agent writes out/audit.json. They all feed the
// SAME strict reader (DisallowUnknownFields), so each must state the exact-shape rule.
var auditJSONPrompts = []string{"audit.md", "audit_verify.md"}

// A book whose audit PASSED was parked because the verify prompt said only "write
// out/audit.json in the normal audit shape" while inviting NIT reporting, so the agent
// emitted {"pass":true,"nit":0,"findings":[]} and the strict reader rejected the unknown
// key - on every retry, because the prompt never stated the constraint the reader enforces.
// audit.md had been hardened against exactly this; audit_verify.md had drifted from it.
func TestAuditPromptsStateTheExactOutputShape(t *testing.T) {
	for _, name := range auditJSONPrompts {
		b, err := files.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		body := string(b)
		for _, required := range []string{
			"EXACTLY",       // the two-field rule is stated, not implied
			"`nit` field",   // the specific key agents reach for is named
			"top-level key", // and the rule is general, not just about nit
		} {
			if !strings.Contains(body, required) {
				t.Errorf("%s must state the exact audit.json output shape (missing %q): a prompt that "+
					"omits it parks a passing book on an unknown-field decode error", name, required)
			}
		}
	}
}

// TestFactPassBranchesShareTheSpoilerContract is the guard on a forked prompt.
//
// factpass.md carries two source-specific branches, and the temptation is to split
// it into two templates. That would be the hand-mirrored-contract failure mode: the
// rules that actually bound spoilers - the chapter range, the one-heading-per-chapter
// output contract, exact chapter attribution, own-words - would then live in two
// places and drift silently, and nothing downstream re-derives them.
//
// So the branches are conditionals inside ONE file, and this test renders both and
// asserts every load-bearing sentence appears in each.
func TestFactPassBranchesShareTheSpoilerContract(t *testing.T) {
	type data struct {
		Title         string
		From          int
		To            int
		HasInherited  bool
		ChunkNote     string
		SpellingSheet string
		IsEbook       bool
		TextDir       string
	}
	audio, err := Render("factpass.md", data{
		Title: "A Book", From: 5, To: 9,
		SpellingSheet: "facts/spellings-through-ch9.md", TextDir: "transcripts-corrected",
	})
	if err != nil {
		t.Fatalf("render audio: %v", err)
	}
	book, err := Render("factpass.md", data{
		Title: "A Book", From: 5, To: 9, IsEbook: true, TextDir: "ebook-text",
	})
	if err != nil {
		t.Fatalf("render ebook: %v", err)
	}

	shared := []string{
		"out/facts-ch5-9.md",                       // the output contract
		"## Chapter N",                             // the heading contract fact_pass validates
		"and none outside that range",              // the spoiler scope
		"Read no chapter beyond 9",                 // the spoiler scope again
		"Exact chapter attribution",                // what makes a reveal position honest
		"Write every fact in fresh, concise words", // the own-words rule
		"Facts only, in neutral reference-guide language",
		"Hyphens only, never em dashes",
		"chapters 5 through 9 only",
		"Do not use the web or your own knowledge of the book",
	}
	for _, want := range shared {
		if !strings.Contains(audio, want) {
			t.Errorf("audio branch is missing the shared rule %q", want)
		}
		if !strings.Contains(book, want) {
			t.Errorf("ebook branch is missing the shared rule %q", want)
		}
	}

	// The source-specific halves must NOT cross over. ASR defensiveness in an ebook
	// prompt would have the agent replace readable names with role labels.
	audioOnly := []string{"NEEDS AUDIO REVIEW", "homophones", "ASR transcripts", "spellings-through-ch9.md"}
	for _, s := range audioOnly {
		if !strings.Contains(audio, s) {
			t.Errorf("audio branch lost %q", s)
		}
		if strings.Contains(book, s) {
			t.Errorf("ebook branch wrongly carries the audio rule %q", s)
		}
	}
	ebookOnly := []string{"the book's own text, exactly as published", "never substitute a\nrole label"}
	for _, s := range ebookOnly {
		if !strings.Contains(book, s) {
			t.Errorf("ebook branch is missing %q", s)
		}
		if strings.Contains(audio, s) {
			t.Errorf("audio branch wrongly carries the ebook rule %q", s)
		}
	}

	// Each branch names only its own staged directory.
	if !strings.Contains(audio, "transcripts-corrected/") || strings.Contains(audio, "ebook-text/") {
		t.Error("audio branch names the wrong text directory")
	}
	if !strings.Contains(book, "ebook-text/") || strings.Contains(book, "transcripts-corrected/") {
		t.Error("ebook branch names the wrong text directory")
	}
}
