package spelling

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// RuleFire is a rule paired with its total replacement count across all chapters
// (Count 0 = the rule never fired).
type RuleFire struct {
	Rule  Rule
	Count int
}

// ChapterFire records a chapter that had at least one replacement.
type ChapterFire struct {
	Stem   string // "ch001"
	Count  int
	Source string // "repaired" | "text"
}

// ApplyResult reports what Apply did, for the caller/tests. Log is the exact
// corrections.log content Apply wrote.
type ApplyResult struct {
	Chapters     int
	Replacements int
	RulesFired   int
	Rules        []RuleFire
	PerChapter   []ChapterFire
	Unfired      []Rule
	Log          string
}

// Apply builds transcripts-corrected/ from transcripts-text/ (preferring a
// transcripts-repaired/ copy when one exists), applying every rule in data ORDER
// via regexp2 - the ordering is the phantom-noble guard (whole phrases before bare
// names). It never touches transcripts-raw/, transcripts-text/ or
// transcripts-repaired/. It writes corrections.log and returns the counts.
//
// Ports apply_corrections.py: per-rule per-chapter counts follow Python re.subn
// (each rule applied to the evolving chapter text, counting its own substitutions).
func Apply(workDir string, data *Corrections) (*ApplyResult, error) {
	compiled := make([]*regexp2.Regexp, len(data.Rules))
	for i, r := range data.Rules {
		re, err := compileRule(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %d pattern %q: %w", i, r.Pattern, err)
		}
		compiled[i] = re
	}

	textDir := filepath.Join(workDir, transcript.TextDir)
	repDir := filepath.Join(workDir, transcript.RepairedDir)
	outDir := filepath.Join(workDir, CorrectedDir)

	names, err := listChapterTxt(textDir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", transcript.TextDir, err)
	}

	counts := make(map[string]int) // keyed by pattern, like Python's counts dict
	var perChapter []ChapterFire
	total := 0

	for _, n := range names {
		srcPath := filepath.Join(textDir, n)
		source := "text"
		if repPath := filepath.Join(repDir, n); fsutil.IsFile(repPath) {
			srcPath = repPath
			source = "repaired"
		}
		b, err := os.ReadFile(srcPath) //nolint:gosec // path derives from the book's work dir
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", srcPath, err)
		}
		text := string(b)
		chapterTotal := 0
		for i, r := range data.Rules {
			k, err := countMatches(compiled[i], text)
			if err != nil {
				return nil, fmt.Errorf("rule %d (%q): %w", i, r.Pattern, err)
			}
			if k == 0 {
				continue
			}
			out, err := compiled[i].Replace(text, r.Replacement, -1, -1)
			if err != nil {
				return nil, fmt.Errorf("rule %d (%q): %w", i, r.Pattern, err)
			}
			text = out
			counts[r.Pattern] += k
			chapterTotal += k
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(outDir, n), []byte(text), 0o644); err != nil {
			return nil, fmt.Errorf("write corrected %s: %w", n, err)
		}
		total += chapterTotal
		if chapterTotal > 0 {
			perChapter = append(perChapter, ChapterFire{
				Stem:   strings.TrimSuffix(n, ".txt"),
				Count:  chapterTotal,
				Source: source,
			})
		}
	}

	rules := make([]RuleFire, len(data.Rules))
	var unfired []Rule
	for i, r := range data.Rules {
		c, ok := counts[r.Pattern]
		rules[i] = RuleFire{Rule: r, Count: c}
		if !ok {
			unfired = append(unfired, r)
		}
	}

	log := buildLog(data.Rules, counts, names, total, perChapter)
	if err := fsutil.WriteFileAtomic(filepath.Join(workDir, "corrections.log"), []byte(log), 0o644); err != nil {
		return nil, fmt.Errorf("write corrections.log: %w", err)
	}

	return &ApplyResult{
		Chapters:     len(names),
		Replacements: total,
		RulesFired:   len(counts),
		Rules:        rules,
		PerChapter:   perChapter,
		Unfired:      unfired,
		Log:          log,
	}, nil
}

// buildLog renders corrections.log in apply_corrections.py's exact structure. The
// rule replacement is shown verbatim ($1 group syntax), a deliberate difference
// from the Python log (which showed the \1 form) since the JSON contract is $-style.
func buildLog(rules []Rule, counts map[string]int, names []string, total int, perChapter []ChapterFire) string {
	lines := []string{
		"# corrections log (raw + text layers are immutable)",
		"",
		fmt.Sprintf("chapters: %d, replacements: %d, rules fired: %d/%d",
			len(names), total, len(counts), len(rules)),
		"",
	}
	for _, r := range rules {
		lines = append(lines, fmt.Sprintf("- %s -> %s  x%d  # %s", r.Pattern, r.Replacement, counts[r.Pattern], r.Note))
	}
	lines = append(lines, "", "## per chapter")
	for _, pc := range perChapter {
		lines = append(lines, fmt.Sprintf("- %s (%s): %d", pc.Stem, pc.Source, pc.Count))
	}
	return strings.Join(lines, "\n") + "\n"
}
