// Package spelling ports the name-correction and spelling-verification ENGINES
// from the historical EXTRACTION-AUDIO.md step 4 ("verify names before
// synthesis") tooling - the per-book Python scripts the contributor pipeline used
// to converge an audiobook's messy ASR spellings onto a single verified
// orthography before the fact pass runs. Four scripts are ported here:
//
//	apply_corrections.py   -> Apply          (rules in data order -> transcripts-corrected/)
//	check_corrections.py   -> Check          (the four gates over the corrected layer)
//	generate_spellings.py  -> GenerateSheets (spoiler-gated per-chunk spelling sheets)
//	check_first_use.py     -> CheckFirstUse  (roster first-appearance cross-check)
//
// # Engine vs data
//
// The Python scripts each embedded a large per-BOOK data table (the RULES list,
// the LEDGER, the CHUNK_ENDS, the clusters). This port splits the two: the ENGINE
// (ordering, gates, occurrence counting, sheet format) lives here and is identical
// for every book; the per-book DATA lives in two JSON files in the book's work dir
// (corrections.json and spellings.json), which M5's agent stages generate. See
// Corrections and Spellings for the contracts.
//
// # The four historical defects this port keeps impossible
//
// Each was a real forgery a rule (not the ASR) committed, and each is guarded by a
// specific mechanism the tests reproduce:
//
//  1. The Owalyn forgery (Book 2): a bare "Owlin -> Owalyn" rule forged a
//     "Priestess Owalyn" who does not exist. Guard: Check gate 3 - every rule's
//     right-hand base must be ATTESTED somewhere in the reference corpus, else "A
//     RULE MAY HAVE INVENTED IT".
//  2. The d'Daston phantom-split (Book 3): a bare "Aston -> Daston" rule matched
//     INSIDE "d'Aston" (an apostrophe is a non-word char, so a word boundary sits
//     there) and forged "Countess d'Daston" x69. Guards: rules apply strictly in
//     data order (whole phrases before bare names - Apply never reorders), and
//     Check gate 4 fails if any particle-plus-surname phantom noble survives.
//  3. The Book 2 cascade: rules were edited but Apply was never re-run, so verified
//     names silently vanished from the gated sheets (first_use is computed FROM the
//     corrected layer). Guard: Check gate 1 - every rule's left-hand pattern must
//     reach ZERO matches in the corrected layer.
//  4. The word-boundary-inside-an-apostrophe pitfall applies to Go's regexp too, so
//     term OCCURRENCE counting never uses \b: Occurrences ports the Python lookaround
//     semantics (?<![A-Za-z])term(?![A-Za-z]) as a plain-string boundary scan, which
//     correctly counts a term that ends in an apostrophe or contains spaces.
//
// # Why regexp2, not the standard library
//
// Rule PATTERNS are agent/user-authored data and use Python-style regex including
// lookbehinds - the real HW05 phantom-noble rule is "(?<!\w)(?:de|d')\s?...". Go's
// stdlib regexp (RE2) cannot express a lookbehind, so rule patterns compile and
// apply through github.com/dlclark/regexp2. Its replacement-group syntax is
// regexp2/.NET style ($1, not the Python \1) - the JSON contract uses $1, and the
// golden-test exporter converts the historical \1 rules. A MatchTimeout bounds each
// compiled pattern so a pathological agent-authored pattern cannot hang the daemon.
// The RE2-compatible phantom-noble scan (Check gate 4) and the roster parser
// (CheckFirstUse) use the stdlib regexp deliberately - they need no lookaround.
package spelling

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/kodestar/audiosilo-meta/pkg/scan"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// Work-dir subdirectories and files the engines read and write. The transcript
// layer names (transcripts-text/, transcripts-repaired/) come from
// internal/transcript, the single source of truth for the layer catalog; the
// layers this package OWNS are declared here.
const (
	// CorrectedDir holds the corrected layer Apply writes and Check/GenerateSheets read.
	CorrectedDir = "transcripts-corrected"
	// FactsDir holds the generated spelling sheets.
	FactsDir = "facts"
	// CorrectionsFile is the per-book correction data.
	CorrectionsFile = "corrections.json"
	// SpellingsFile is the per-book spelling-sheet data.
	SpellingsFile = "spellings.json"
)

// ruleTimeout bounds a single regexp2 match/replace so a pathological
// agent-authored pattern cannot hang the daemon. regexp2 returns the timeout as an
// error, which the engines surface as a rule error.
const ruleTimeout = 5 * time.Second

// Sentinel errors so the pipeline can branch on a missing data file.
var (
	// ErrNoCorrections is returned (wrapped) by LoadCorrections when the file is absent.
	ErrNoCorrections = errors.New("corrections.json not found")
	// ErrNoSpellings is returned (wrapped) by LoadSpellings when the file is absent.
	ErrNoSpellings = errors.New("spellings.json not found")
)

// Rule is one correction: a regexp2 pattern, a regexp2/.NET-style replacement
// ($1 group refs), and a human note carried into the corrections log. Rules apply
// in array order - a hard contract, so whole-phrase rules run before bare-name
// rules (the d'Daston guard).
type Rule struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	Note        string `json:"note"`
}

// Corrections is the corrections.json contract: the ordered rule list, the
// deliberately-unresolved names (as-heard, never merged), and optional extra
// attestation-corpus sources for Check gate 3.
type Corrections struct {
	Rules      []Rule   `json:"rules"`
	Unresolved []string `json:"unresolved"`
	// ReferenceFiles are the attestation sources for Check gate 3 beyond the
	// corrected layer itself, each a file or directory path, absolute or relative
	// to the work dir. A directory contributes all its *.txt files
	// (natural/numeric-aware order). Typical entries: the work dir's
	// marker_titles.txt (the tier-1 embedded chapter-marker titles) and a
	// published-text mirror. Nothing is consulted implicitly - the list IS the
	// contract, so an M5 agent (and a reader of the JSON) sees every source.
	ReferenceFiles []string `json:"reference_files"`
}

// Validate rejects an empty pattern/replacement, a pattern that does not compile,
// and a replacement whose group syntax the Check gates cannot attest. Gates 2/3
// derive the bare name to attest by stripping a LEADING "$1 " (deriveBase); any
// other group reference ("$2", "${1}", "${name}", a mid-string "$1") would survive
// into the derived base, never occur literally in any corpus, and fail the gates
// as a false "A RULE MAY HAVE INVENTED IT" - so it is refused loudly at load time
// instead. "$$" (regexp2's literal-dollar escape) is allowed anywhere.
func (c *Corrections) Validate() error {
	for i, r := range c.Rules {
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("rule %d: empty pattern", i)
		}
		if r.Replacement == "" {
			return fmt.Errorf("rule %d: empty replacement", i)
		}
		if _, err := compileRule(r.Pattern); err != nil {
			return fmt.Errorf("rule %d: bad pattern %q: %w", i, r.Pattern, err)
		}
		rest := strings.TrimPrefix(r.Replacement, "$1 ")
		if strings.Contains(strings.ReplaceAll(rest, "$$", ""), "$") {
			return fmt.Errorf("rule %d: replacement %q uses a group reference the check gates cannot attest - only a leading \"$1 \" (title prefix) is supported", i, r.Replacement)
		}
	}
	return nil
}

// LedgerEntry is one verified-spelling row. Carryover rows (names the reader
// already knows from earlier books) are pinned to first_use 0 in the sheets.
type LedgerEntry struct {
	Canonical string `json:"canonical"`
	Type      string `json:"type"`
	Status    string `json:"status"` // "verified" | "probable"
	Carryover bool   `json:"carryover"`
	Variants  string `json:"variants"`
	Note      string `json:"note"`
}

// Cluster is a DO-NOT-MERGE warning: look-alike ASR strings that are distinct
// entities. It is shown in a sheet only once EVERY referenced name has been heard.
type Cluster struct {
	Names []string `json:"names"`
	Text  string   `json:"text"`
}

// NonMerge is a deliberate carryover pair the ASR blurs; shown when both names are
// present ledger canonicals within the sheet's chapter window.
type NonMerge struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Text string `json:"text"`
}

// Spellings is the spellings.json contract driving GenerateSheets. Title and
// Preamble are the book-specific sheet header (data, not engine). ChunkEnds are the
// per-chunk spoiler boundaries.
type Spellings struct {
	Title      string        `json:"title"`
	ChunkEnds  []int         `json:"chunk_ends"`
	Preamble   []string      `json:"preamble"`
	Ledger     []LedgerEntry `json:"ledger"`
	Unresolved []string      `json:"unresolved"`
	Clusters   []Cluster     `json:"clusters"`
	NonMerges  []NonMerge    `json:"non_merges"`
}

// Validate rejects an empty canonical and a chunk_ends list that is not strictly
// increasing positive integers.
func (s *Spellings) Validate() error {
	for i, e := range s.Ledger {
		if strings.TrimSpace(e.Canonical) == "" {
			return fmt.Errorf("ledger %d: empty canonical", i)
		}
	}
	prev := 0
	for i, end := range s.ChunkEnds {
		if end <= 0 {
			return fmt.Errorf("chunk_ends[%d]: must be positive, got %d", i, end)
		}
		if i > 0 && end <= prev {
			return fmt.Errorf("chunk_ends must be strictly increasing: %d does not exceed %d", end, prev)
		}
		prev = end
	}
	return nil
}

// LoadCorrections reads and validates <workDir>/corrections.json. A missing file
// wraps ErrNoCorrections so the pipeline can branch on it.
func LoadCorrections(workDir string) (*Corrections, error) {
	path := filepath.Join(workDir, CorrectionsFile)
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoCorrections, path)
		}
		return nil, err
	}
	var c Corrections
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", CorrectionsFile, err)
	}
	return &c, nil
}

// LoadSpellings reads and validates <workDir>/spellings.json. A missing file wraps
// ErrNoSpellings.
func LoadSpellings(workDir string) (*Spellings, error) {
	path := filepath.Join(workDir, SpellingsFile)
	b, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoSpellings, path)
		}
		return nil, err
	}
	var s Spellings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", SpellingsFile, err)
	}
	return &s, nil
}

// Occurrences counts non-overlapping literal matches of term in hay where the byte
// immediately before and after the match is not an ASCII letter [A-Za-z]. This is
// the exact semantics of the Python lookaround (?<![A-Za-z])term(?![A-Za-z]) that
// every gate and the first-use check use, and deliberately NOT a word boundary: a
// word boundary matches inside an apostrophe (the d'Daston pitfall) and cannot
// bound a term that ends in an apostrophe. Start-of-string and end-of-string count
// as non-letters. Matches Python re.findall's non-overlapping advance (past each
// match). term may contain spaces (a multi-word phrase) and may end in an apostrophe.
func Occurrences(hay, term string) int {
	if term == "" {
		return 0
	}
	count := 0
	for i := 0; i+len(term) <= len(hay); {
		j := strings.Index(hay[i:], term)
		if j < 0 {
			break
		}
		pos := i + j
		beforeOK := pos == 0 || !isASCIILetter(hay[pos-1])
		afterOK := pos+len(term) == len(hay) || !isASCIILetter(hay[pos+len(term)])
		if beforeOK && afterOK {
			count++
			i = pos + len(term) // non-overlapping: advance past the match
		} else {
			i = pos + 1 // boundary failed here; try the next start position
		}
	}
	return count
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// normalizeApostrophes maps the curly apostrophe (U+2019) to the ASCII form, so a
// mirror's typography matches an ASR corpus (ported from the Python reference load).
func normalizeApostrophes(s string) string {
	return strings.ReplaceAll(s, "’", "'")
}

// compileRule compiles a rule pattern with regexp2 and a bounded MatchTimeout.
func compileRule(pattern string) (*regexp2.Regexp, error) {
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = ruleTimeout
	return re, nil
}

// countMatches counts the non-overlapping matches of a compiled rule in s, using
// the same iteration regexp2.Replace uses, so the count equals the substitutions a
// Replace would make (Python re.subn's returned count).
func countMatches(re *regexp2.Regexp, s string) (int, error) {
	n := 0
	m, err := re.FindStringMatch(s)
	if err != nil {
		return 0, err
	}
	for m != nil {
		n++
		m, err = re.FindNextMatch(m)
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// listChapterTxt returns the "chNNN.txt" filenames in dir, lexicographically sorted
// (matching Python's sorted(glob)). Because the stems are zero-padded (ch%03d),
// lexicographic order equals chapter order. Names that do not parse as a chapter
// (transcript.ParseChapter) are excluded, so every consumer - Apply, the layer
// join, and the per-chapter map - agrees on exactly which files are chapters.
func listChapterTxt(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		if _, ok := transcript.ParseChapter(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// correctedChapters reads the corrected layer into a chapter-number-keyed map plus
// the sorted chapter numbers (the shape GenerateSheets and CheckFirstUse iterate).
func correctedChapters(workDir string) (map[int]string, []int, error) {
	dir := filepath.Join(workDir, CorrectedDir)
	names, err := listChapterTxt(dir)
	if err != nil {
		return nil, nil, err
	}
	m := make(map[int]string, len(names))
	for _, n := range names {
		num, ok := transcript.ParseChapter(n)
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // path derives from the book's work dir
		if err != nil {
			return nil, nil, err
		}
		m[num] = string(b)
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return m, keys, nil
}

// correctedLayer joins the corrected layer's chapter files (lexicographic filename
// order) with single spaces - the reference/gate substrate (Python's " ".join).
func correctedLayer(workDir string) (string, error) {
	dir := filepath.Join(workDir, CorrectedDir)
	names, err := listChapterTxt(dir)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // path derives from the book's work dir
		if err != nil {
			return "", err
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, " "), nil
}

// readReferenceSource reads a gate-3 reference source at absPath. A directory
// contributes all its *.txt files sorted with the shared natural comparator
// (scan.NaturalLess - the same ordering the audio split uses, no local copy). A
// missing path contributes nothing (graceful, like the Python's optional mirror).
// All text is curly-apostrophe normalized.
func readReferenceSource(absPath string) (string, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(absPath) //nolint:gosec // reference path is operator-authored data
		if err != nil {
			return "", err
		}
		return normalizeApostrophes(string(b)), nil
	}
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return scan.NaturalLess(names[i], names[j]) })
	parts := make([]string, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(absPath, n)) //nolint:gosec // reference path is operator-authored data
		if err != nil {
			return "", err
		}
		parts = append(parts, normalizeApostrophes(string(b)))
	}
	return strings.Join(parts, " "), nil
}

// resolveRefPath resolves a reference entry (absolute, or relative to workDir) to a
// cleaned absolute path for reading and de-duplication.
func resolveRefPath(workDir, entry string) string {
	p := entry
	if !filepath.IsAbs(p) {
		p = filepath.Join(workDir, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}
