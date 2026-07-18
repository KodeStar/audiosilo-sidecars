package spelling

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// DeadRules returns the rules in corr whose pattern matches NOTHING in the book's
// ORIGINAL source layer - the transcript BEFORE any correction is applied
// (transcripts-text/, preferring a transcripts-repaired/ copy per chapter, exactly
// the layer Apply reads first). A dead rule is a silent no-op that ships as
// under-correction, and neither Check gate reliably catches it: gate 1 is happy with
// zero LHS matches, and gates 2/3 pass whenever the canonical RHS is attested
// elsewhere - the common case, since the agent picks the RHS precisely because it is
// a high-count form. So the strongest gate for the real dead-rule shape lives here.
//
// It matches against the ORIGINAL layer, NOT the evolving/corrected text, on purpose:
// rules apply in array order to evolving text, so an earlier whole-phrase rule can
// legitimately shadow a later bare-name rule to zero fires while that bare name still
// occurs in the source (the whole-phrase-before-bare-name ordering the d'Daston guard
// requires). Counting against the original accepts those order-shadowed rules and
// flags only patterns that never occur at all.
//
// It is deliberately NOT folded into Check: Check's four gates are a contract-frozen
// port, golden-tested against historical books, and adding a gate could break those
// fixtures. DeadRules is a separate validator the spelling_research stage calls.
func DeadRules(workDir string, corr *Corrections) ([]Rule, error) {
	names, err := listChapterTxt(filepath.Join(workDir, transcript.TextDir))
	if err != nil {
		return nil, err
	}
	// The original source corpus: each chapter's preferred (repaired-over-text) layer,
	// the substrate Apply reads BEFORE applying any rule. Counted per chapter (never
	// space-joined) so a match can never straddle a chapter boundary.
	var chapters []string
	for _, n := range names {
		num, ok := transcript.ParseChapter(n)
		if !ok {
			continue
		}
		src, ok := transcript.ChapterTextPath(workDir, num)
		if !ok {
			continue
		}
		b, rerr := os.ReadFile(src) //nolint:gosec // path derives from the book's work dir
		if rerr != nil {
			return nil, rerr
		}
		chapters = append(chapters, normalizeApostrophes(string(b)))
	}

	var dead []Rule
	for _, r := range corr.Rules {
		re, cerr := compileRule(r.Pattern)
		if cerr != nil {
			return nil, fmt.Errorf("rule pattern %q: %w", r.Pattern, cerr)
		}
		matched := false
		for _, text := range chapters {
			k, kerr := countMatches(re, text)
			if kerr != nil {
				return nil, fmt.Errorf("rule pattern %q: %w", r.Pattern, kerr)
			}
			if k > 0 {
				matched = true
				break
			}
		}
		if !matched {
			dead = append(dead, r)
		}
	}
	return dead, nil
}
