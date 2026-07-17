package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// The sidecar work-dir layout the synthesis/validating/auditing/fixing stages share.
// The two sidecar JSON files live under sidecars/ so a scratch purge or a manual
// inspection finds them together; audit.json and validation_report.json ride in the
// work-dir root next to the other stage artifacts.
const (
	sidecarsDir           = "sidecars"
	charactersFileName    = "characters.json"
	recapsFileName        = "recaps.json"
	validationReportName  = "validation_report.json"
	auditReportName       = "audit.json"
	sidecarLicenseContent = "CC-BY-SA-3.0"
	sourceTypeCommunity   = "community"
)

// Expressive-field length caps (Unicode code points), mirrored from the
// audiosilo-meta characters/recaps schemas. A cap breach is a validation error, so a
// too-long entry is caught here rather than at contribution time.
const (
	capDescription = 1500
	capRecapText   = 3000
	capInShort     = 1500
	capEnding      = 2000
)

// emDash is the U+2014 character the house style forbids in generated prose. It is
// an error (not a warning) in any expressive sidecar field so the CC BY-SA text this
// tool contributes never carries an AI-tell em dash.
const emDash = '—'

// Audit severities (out/audit.json). Consistency between the agent's self-reported
// pass flag and its findings is enforced in code (a Pass=true with a BLOCKER/FIX is
// overridden), so these are a closed enum the validator checks.
const (
	SeverityBlocker = "BLOCKER"
	SeverityFix     = "FIX"
	SeverityNit     = "NIT"
)

// wikidataRe is the character xref.wikidata QID pattern from the characters schema.
var wikidataRe = regexp.MustCompile(`^Q\d+$`)

var (
	validRoles  = map[string]bool{"protagonist": true, "antagonist": true, "supporting": true, "minor": true}
	validScopes = map[string]bool{"book": true, "series": true}
)

// The sidecar files ARE the audiosilo-meta pkg/model entity types (the upstream
// contract this tool contributes into): characters.json is a model.Characters and
// recaps.json is a model.Recaps. The rule validation below (caps, em dash, enums,
// exactly-one-community source, kebab-case ids via model.ValidSlug) is enforced here
// as functions over those upstream types; the strict decode wrapper below keeps the
// DisallowUnknownFields + trailing-content guard.

// decodeSidecarFile reads path and decodes it into v with DisallowUnknownFields, so
// an agent that invents a field (a spoiler-carrying extra key, a mis-shaped position)
// fails fast rather than silently dropping data. It also rejects trailing content
// after the top-level value.
func decodeSidecarFile(path string, v any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing content after JSON value")
		}
		return err
	}
	return nil
}

// runeLen counts Unicode code points, matching the JSON-schema maxLength semantics
// the caps enforce.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// hasEmDash reports whether s contains the forbidden U+2014 em dash.
func hasEmDash(s string) bool { return strings.ContainsRune(s, emDash) }

// validateSidecars runs every structural rule the synthesis/fix validators require
// and the validating stage surfaces. errs are hard contract breaches (a synthesis or
// fix retry must clear all of them); warns are advisory (the missing book-2 series
// recap) that ride into auditing but never block. chapterCount is the manifest's
// logical chapter count; seriesOpener gates the chapter-0 series-recap rule.
func validateSidecars(chars *model.Characters, recs *model.Recaps, chapterCount int, seriesOpener bool) (errs, warns []string) {
	if chars == nil || recs == nil {
		return []string{"characters.json and recaps.json are both required"}, nil
	}

	// characters.json
	if !model.ValidSlug(chars.Work) {
		errs = append(errs, fmt.Sprintf("characters.json: work %q is not a kebab-case slug", chars.Work))
	}
	if chars.License != sidecarLicenseContent {
		errs = append(errs, fmt.Sprintf("characters.json: license must be %q, got %q", sidecarLicenseContent, chars.License))
	}
	errs = append(errs, validateSources("characters.json", chars.Sources)...)
	if len(chars.Characters) == 0 {
		errs = append(errs, "characters.json: at least one character is required")
	}
	seenID := make(map[string]bool, len(chars.Characters))
	for i, c := range chars.Characters {
		locus := fmt.Sprintf("characters[%d]", i)
		if !model.ValidSlug(c.ID) {
			errs = append(errs, fmt.Sprintf("%s.id %q is not a kebab-case slug", locus, c.ID))
		} else if seenID[c.ID] {
			errs = append(errs, fmt.Sprintf("%s.id %q is not unique within the file", locus, c.ID))
		}
		seenID[c.ID] = true
		if strings.TrimSpace(c.Name) == "" {
			errs = append(errs, locus+".name is empty")
		}
		if c.Role != "" && !validRoles[c.Role] {
			errs = append(errs, fmt.Sprintf("%s.role %q is not one of protagonist/antagonist/supporting/minor", locus, c.Role))
		}
		errs = append(errs, validateChapter(locus+".reveal", c.Reveal.Chapter, chapterCount)...)
		if strings.TrimSpace(c.Description) == "" {
			errs = append(errs, locus+".description is empty")
		}
		if runeLen(c.Description) > capDescription {
			errs = append(errs, fmt.Sprintf("%s.description is %d chars, over the %d cap", locus, runeLen(c.Description), capDescription))
		}
		if hasEmDash(c.Description) {
			errs = append(errs, locus+".description contains an em dash (use hyphens)")
		}
		for j, a := range c.Aliases {
			if strings.TrimSpace(a) == "" {
				errs = append(errs, fmt.Sprintf("%s.aliases[%d] is empty", locus, j))
			}
		}
		if c.Xref != nil && c.Xref.Wikidata != "" && !wikidataRe.MatchString(c.Xref.Wikidata) {
			errs = append(errs, fmt.Sprintf("%s.xref.wikidata %q is not a QID", locus, c.Xref.Wikidata))
		}
	}

	// recaps.json
	if !model.ValidSlug(recs.Work) {
		errs = append(errs, fmt.Sprintf("recaps.json: work %q is not a kebab-case slug", recs.Work))
	}
	if recs.License != sidecarLicenseContent {
		errs = append(errs, fmt.Sprintf("recaps.json: license must be %q, got %q", sidecarLicenseContent, recs.License))
	}
	errs = append(errs, validateSources("recaps.json", recs.Sources)...)
	if len(recs.Recaps) == 0 {
		errs = append(errs, "recaps.json: at least one recap is required")
	}
	seenThrough := make(map[int]bool, len(recs.Recaps))
	var seriesRecap0 bool
	for i, r := range recs.Recaps {
		locus := fmt.Sprintf("recaps[%d]", i)
		errs = append(errs, validateChapter(locus+".through", r.Through.Chapter, chapterCount)...)
		if seenThrough[r.Through.Chapter] {
			errs = append(errs, fmt.Sprintf("%s.through.chapter %d is not unique within the file", locus, r.Through.Chapter))
		}
		seenThrough[r.Through.Chapter] = true
		if strings.TrimSpace(r.Text) == "" {
			errs = append(errs, locus+".text is empty")
		}
		if runeLen(r.Text) > capRecapText {
			errs = append(errs, fmt.Sprintf("%s.text is %d chars, over the %d cap", locus, runeLen(r.Text), capRecapText))
		}
		if hasEmDash(r.Text) {
			errs = append(errs, locus+".text contains an em dash (use hyphens)")
		}
		if r.Scope != "" && !validScopes[r.Scope] {
			errs = append(errs, fmt.Sprintf("%s.scope %q is not book/series", locus, r.Scope))
		}
		if r.Through.Chapter == 0 && r.Scope == "series" {
			seriesRecap0 = true
		}
	}
	errs = append(errs, validateSummary("recaps.json in_short", recs.InShort, capInShort)...)
	errs = append(errs, validateSummary("recaps.json ending", recs.Ending, capEnding)...)

	// The chapter-0 series recap ("previously, in earlier books") is a book-2+ device.
	if seriesOpener && seriesRecap0 {
		errs = append(errs, "recaps.json: a series opener must NOT carry a chapter:0 scope:series recap")
	}
	if !seriesOpener && !seriesRecap0 {
		warns = append(warns, "recaps.json: a non-opener should carry a chapter:0 scope:series \"previously\" recap")
	}
	return errs, warns
}

// validateSources enforces the exactly-one-community-source contract.
func validateSources(file string, sources []model.Source) []string {
	if len(sources) != 1 || sources[0].Type != sourceTypeCommunity {
		return []string{fmt.Sprintf("%s: sources must be exactly [{\"type\":\"community\"}]", file)}
	}
	return nil
}

// validateChapter checks a position chapter is within [0, chapterCount].
func validateChapter(locus string, chapter, chapterCount int) []string {
	if chapter < 0 || chapter > chapterCount {
		return []string{fmt.Sprintf("%s.chapter %d is outside the range 0..%d", locus, chapter, chapterCount)}
	}
	return nil
}

// validateSummary checks an optional whole-book summary field's cap and em-dash rule.
func validateSummary(locus, text string, limit int) []string {
	if text == "" {
		return nil
	}
	var errs []string
	if runeLen(text) > limit {
		errs = append(errs, fmt.Sprintf("%s is %d chars, over the %d cap", locus, runeLen(text), limit))
	}
	if hasEmDash(text) {
		errs = append(errs, locus+" contains an em dash (use hyphens)")
	}
	return errs
}

// verifiedLedgerTable renders the book's verified spellings as a markdown table
// (canonical | type | aliases/variants), for the synthesis/audit/fix prompts to
// pin proper-noun spellings. It reads spellings.json via the spelling engine's
// loader; a missing file or no verified entries yields "" (the prompt then omits the
// ledger block). A malformed spellings.json is a real error.
func verifiedLedgerTable(workDir string) (string, error) {
	sp, err := spelling.LoadSpellings(workDir)
	if err != nil {
		if errors.Is(err, spelling.ErrNoSpellings) {
			return "", nil
		}
		return "", err
	}
	var rows []spelling.LedgerEntry
	for _, e := range sp.Ledger {
		if e.Status == "verified" {
			rows = append(rows, e)
		}
	}
	if len(rows) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("| canonical | type | aliases/variants |\n")
	b.WriteString("| --- | --- | --- |\n")
	for _, e := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(e.Canonical), cell(e.Type), cell(e.Variants))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// cell escapes a markdown table cell (pipes and newlines) so a ledger value with a
// pipe cannot break the rendered table.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// workSlug is the single sidecars work-slug derivation shared by synthesis, the
// contributing stage, and the local export (ExportSlug forwards to it): the book's
// matched work id when it is a valid meta slug, else a kebab-case slug of the title,
// else "book". Applying model.ValidSlug to the stored WorkID means an INVALID id falls
// back to the title (the tightened, correct semantics) rather than shipping a
// non-slug work. validating tolerates a placeholder slug; the contribution stage (M7)
// reconciles it against the real work.
func workSlug(book store.Book) string {
	if s := strings.TrimSpace(book.WorkID); model.ValidSlug(s) {
		return s
	}
	if s := slugify(book.Title); s != "" {
		return s
	}
	return "book"
}

// authors renders a book's authors for a prompt (comma-joined), defaulting to
// "Unknown" so no prompt template ever renders an empty by-line. Shared by every
// agent-stage prompt-data site.
func authors(book store.Book) string {
	if len(book.Authors) == 0 {
		return "Unknown"
	}
	return strings.Join(book.Authors, ", ")
}

// slugify lowercases and hyphenates a title into a kebab-case slug.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// isSeriesOpener reports whether this book opens its series (so it must NOT carry a
// chapter-0 "previously" recap). A seriesless book is trivially an opener; otherwise
// it is an opener when no same-series predecessor exists. findSeriesPredecessor is
// owned by series.go (shared with the spelling/fact-pass carryover discovery).
func (e *Executor) isSeriesOpener(ctx context.Context, book store.Book) (bool, error) {
	if strings.TrimSpace(book.Series) == "" {
		return true, nil
	}
	_, found, err := findSeriesPredecessor(ctx, e.db, book)
	if err != nil {
		return false, err
	}
	return !found, nil
}

// mdFilter selects the .md files under facts/ for staging (the knowledge sheets and
// the spelling sheets - all ledger-derived and safe for the notes-only stages).
func mdFilter(rel string) bool { return strings.HasSuffix(strings.ToLower(rel), ".md") }

// factsDirPath is the book's facts/ directory (the fact pass's per-chapter notes).
func factsDirPath(workDir string) string { return filepath.Join(workDir, spelling.FactsDir) }

// AuditFinding is one entry in the auditor's report (out/audit.json).
type AuditFinding struct {
	Severity   string `json:"severity"`
	Locus      string `json:"locus"`
	Text       string `json:"text"`
	Evidence   string `json:"evidence"`
	Suggestion string `json:"suggestion"`
}

// AuditReport is the auditor's verdict: a self-reported pass plus findings. The
// pipeline re-derives an effective pass in code (a BLOCKER/FIX or an unclean
// validation report overrides an over-optimistic Pass=true).
type AuditReport struct {
	Pass     bool           `json:"pass"`
	Findings []AuditFinding `json:"findings"`
}

// counts tallies the report's findings by severity.
func (r AuditReport) counts() (blocker, fix, nit int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityBlocker:
			blocker++
		case SeverityFix:
			fix++
		case SeverityNit:
			nit++
		}
	}
	return blocker, fix, nit
}

// validationReport is validating's output (validation_report.json): the canonical +
// structural + no-verbatim results, split by severity. Errors are hard contract
// breaches (a malformed sidecar, a cap/em-dash/enum violation, a near-verbatim
// overlap) that the auditor treats as automatic FIX-level and that gate the effective
// audit pass; Warnings are advisory (a non-opener missing its chapter:0 series recap)
// that the auditor sees as context but that must NOT block a pass. Clean is
// len(Errors)==0 - a warning-only report is clean. Both lists ride into auditing; the
// stage never parks on them.
type validationReport struct {
	Clean    bool     `json:"clean"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}
