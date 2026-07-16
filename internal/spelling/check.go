package spelling

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dlclark/regexp2"
)

// CheckFailureKind categorizes a Check failure so callers can act on it.
type CheckFailureKind string

const (
	// FailLHSSurvives - a rule's pattern still matches the corrected layer (gate 1).
	FailLHSSurvives CheckFailureKind = "lhs_survives"
	// FailRHSAbsent - a rule's right-hand base never occurs in the corrected layer (gate 2).
	FailRHSAbsent CheckFailureKind = "rhs_absent"
	// FailRHSUnattested - a rule's right-hand base is not attested in the reference union (gate 3).
	FailRHSUnattested CheckFailureKind = "rhs_unattested"
	// FailPhantomNoble - a particle-plus-surname phantom noble survives (gate 4).
	FailPhantomNoble CheckFailureKind = "phantom_noble"
)

// CheckFailure is one gate violation. Rule is set for the rule-scoped gates
// (nil for the phantom-noble scan); Base is the derived right-hand base for gates 2/3.
type CheckFailure struct {
	Kind    CheckFailureKind
	Rule    *Rule
	Base    string
	Message string
}

// CheckResult is the outcome of Check over the corrected layer.
type CheckResult struct {
	RulesChecked int
	LayerWords   int
	Failures     []CheckFailure
}

// Ok reports whether every gate passed.
func (r *CheckResult) Ok() bool { return len(r.Failures) == 0 }

// Summary renders a multi-line report in the spirit of check_corrections.py's output.
func (r *CheckResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rules checked: %d\n", r.RulesChecked)
	fmt.Fprintf(&b, "corrected layer: %d words\n", r.LayerWords)
	if len(r.Failures) == 0 {
		b.WriteString("\nOK: every LHS is gone, every RHS is present in the layer AND attested " +
			"in the reference union, and no phantom nobles survive.")
		return b.String()
	}
	fmt.Fprintf(&b, "\nFAILURES (%d):", len(r.Failures))
	for _, f := range r.Failures {
		fmt.Fprintf(&b, "\n  - %s", f.Message)
	}
	return b.String()
}

// phantomNobleRe is the gate-4 scan (Book 3's failure shape). It needs no
// lookaround, so it uses the stdlib regexp (RE2).
var phantomNobleRe = regexp.MustCompile(`\b(?:Countess|Count|Lady|Lord)\s+(?:de|d')\s?[A-Z]\w*`)

// Check post-checks the corrected layer, porting check_corrections.py's four gates:
//
//  1. every rule's LHS pattern reaches ZERO in the corrected layer (the cascade guard);
//  2. every rule's RHS base occurs in the corrected layer (dead-rule guard);
//  3. every rule's RHS base is attested in the reference union - the corrected layer
//     plus the reference_files (the invented-name guard);
//  4. no particle-plus-surname phantom noble survives.
//
// The reference base for gates 2/3 is the replacement with the "$1 " prefix and any
// "'s" removed (deriveBase). All reference text is curly-apostrophe normalized.
func Check(workDir string, data *Corrections) (*CheckResult, error) {
	compiled := make([]*regexp2.Regexp, len(data.Rules))
	for i, r := range data.Rules {
		re, err := compileRule(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %d pattern %q: %w", i, r.Pattern, err)
		}
		compiled[i] = re
	}

	layer, err := correctedLayer(workDir)
	if err != nil {
		return nil, fmt.Errorf("read corrected layer: %w", err)
	}

	reference, err := buildReference(workDir, layer, data.ReferenceFiles)
	if err != nil {
		return nil, err
	}

	result := &CheckResult{
		RulesChecked: len(data.Rules),
		LayerWords:   len(strings.Fields(layer)),
	}

	// Gate 1: every LHS is gone from the corrected layer.
	for i := range data.Rules {
		n, err := countMatches(compiled[i], layer)
		if err != nil {
			return nil, fmt.Errorf("gate 1 rule %d (%q): %w", i, data.Rules[i].Pattern, err)
		}
		if n > 0 {
			result.Failures = append(result.Failures, CheckFailure{
				Kind: FailLHSSurvives,
				Rule: &data.Rules[i],
				Message: fmt.Sprintf("LHS SURVIVES: %s still matches %dx -> rule never applied?",
					data.Rules[i].Pattern, n),
			})
		}
	}

	// Gates 2 + 3: every RHS base occurs in the layer AND is attested in the union.
	for i := range data.Rules {
		base := deriveBase(data.Rules[i].Replacement)
		if Occurrences(layer, base) == 0 {
			result.Failures = append(result.Failures, CheckFailure{
				Kind: FailRHSAbsent,
				Rule: &data.Rules[i],
				Base: base,
				Message: fmt.Sprintf("RHS ABSENT from corrected layer: %q (rule %s) - dead rule?",
					base, data.Rules[i].Pattern),
			})
		}
		if Occurrences(reference, base) == 0 {
			result.Failures = append(result.Failures, CheckFailure{
				Kind: FailRHSUnattested,
				Rule: &data.Rules[i],
				Base: base,
				Message: fmt.Sprintf("RHS NOT ATTESTED in the reference union (layer + reference "+
					"files): %q - A RULE MAY HAVE INVENTED IT (rule %s)", base, data.Rules[i].Pattern),
			})
		}
	}

	// Gate 4: phantom-noble particle forms (Book 3's shape).
	if phantoms := phantomNobleRe.FindAllString(layer, -1); len(phantoms) > 0 {
		result.Failures = append(result.Failures, CheckFailure{
			Kind:    FailPhantomNoble,
			Message: "PARTICLE NOBLES SURVIVE: " + topCounts(phantoms, 5),
		})
	}

	return result, nil
}

// deriveBase reduces a replacement to the bare name Check gates 2/3 attest: it
// removes the "$1 " title-preserving prefix and every "'s" possessive, faithfully
// porting the Python repl.replace("\\1 ", "").replace("'s", "") (global str.replace)
// under the regexp2/.NET $1 group syntax.
func deriveBase(repl string) string {
	b := strings.ReplaceAll(repl, "$1 ", "")
	b = strings.ReplaceAll(b, "'s", "")
	return b
}

// buildReference assembles the gate-3 reference union: the corrected layer plus the
// data-supplied reference_files (in order). Nothing is consulted implicitly - the
// Corrections.ReferenceFiles list IS the attestation contract, so a reader of the
// JSON (and the M5 agent writing it) sees every source; the work dir's
// marker_titles.txt is a typical explicit entry.
func buildReference(workDir, layer string, referenceFiles []string) (string, error) {
	var refParts []string
	for _, entry := range referenceFiles {
		txt, err := readReferenceSource(resolveRefPath(workDir, entry))
		if err != nil {
			return "", fmt.Errorf("reference %q: %w", entry, err)
		}
		if txt != "" {
			refParts = append(refParts, txt)
		}
	}
	if len(refParts) == 0 {
		return layer, nil
	}
	return layer + " " + strings.Join(refParts, " "), nil
}

// topCounts renders the up-to-n most frequent strings (count desc, then string asc)
// as a "[(str, count), ...]" list for the gate-4 message.
func topCounts(items []string, n int) string {
	counter := make(map[string]int)
	for _, it := range items {
		counter[it]++
	}
	keys := make([]string, 0, len(counter))
	for k := range counter {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counter[keys[i]] != counter[keys[j]] {
			return counter[keys[i]] > counter[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("(%q, %d)", k, counter[k]))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
