package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// markerTitlesFile is the recording's chapter-marker titles (one per manifest
// chapter): tier-1 spelling evidence and a Check gate-3 attestation source. The
// spelling_research stage writes it (from the manifest) if the earlier stages have
// not.
const markerTitlesFile = "marker_titles.txt"

// spellingRefsDir holds the series-predecessor carryover the daemon stages for the
// spelling agent (never the agent itself): the previous volume's corrected chapter
// texts plus its prior-* ledger/rules/marker files. It is the ONLY attestation
// source, besides marker_titles.txt, a correction rule may cite.
const spellingRefsDir = "spelling-refs"

// spellingCorpusFloor is the transcript word count above which zero extracted
// candidates is treated as a broken extractor (a hard failure) rather than a
// genuinely tiny book. Below it, an empty shortlist is allowed to proceed.
const spellingCorpusFloor = 5000

// spellingLedgerStatuses is the closed set of ledger statuses the validator accepts.
var spellingLedgerStatuses = map[string]bool{"verified": true, "probable": true, "unresolved": true}

// priorSpellingsFile is the staged name of the predecessor's spelling ledger under
// spelling-refs/. The carryover-integrity validator reads it (via
// spelling.PriorCanonicals) to confirm each carryover:true row names a canonical the
// predecessor actually ledgered.
const priorSpellingsFile = "prior-spellings.json"

// priorRefNames maps each series-predecessor carryover file (src, named in the prior
// book's work dir) to its prior-* staged name (dst). populateSpellingRefs writes them
// by src into spelling-refs/; the spelling_research staging loop reads them back by
// dst. A missing file is skipped silently by design (best-effort carryover).
var priorRefNames = []struct{ src, dst string }{
	{markerTitlesFile, "prior-marker_titles.txt"},
	{spelling.SpellingsFile, priorSpellingsFile},
	{spelling.CorrectionsFile, "prior-corrections.json"},
}

// seriesGlossaryFile is the canonical-name list drawn from the community metadata
// database's OTHER volumes of this book's series. It lives under spelling-refs/, so
// it is already an allowed reference_files citation and already lands in the
// correction gate's attestation corpus - no validator change was needed to let a
// rule attest an upstream-canonical spelling.
const seriesGlossaryFile = "series-glossary.txt"

// spellingPromptData feeds spelling.md. Field names MUST match the template (rendered
// with missingkey=error).
type spellingPromptData struct {
	Title        string
	Authors      string
	Series       string
	SeriesPos    string
	HasCarryover bool
	WebAvailable bool
	ChunkEnds    string
	// HasGlossary reports that spelling-refs/series-glossary.txt was staged, and
	// GlossaryWorks names the sibling volumes it came from (for the prompt's
	// provenance sentence).
	HasGlossary   bool
	GlossaryWorks string
	// HasReferenceMatches reports that the deterministic pre-pass proposed at least
	// one correction against a reference vocabulary.
	HasReferenceMatches bool
	// EvidencePriority is the rendered evidence-priority list (see evidencePriority).
	EvidencePriority string
}

// spellingResearch is the one web-enabled agent stage: it builds the canonical
// spelling ledger (spellings.json) and the mechanical correction rules
// (corrections.json) that turn the raw transcript into a trustworthy corrected layer.
// The daemon does the mechanical pre-work first (marker_titles.txt, chunk_plan.json,
// and the series-predecessor carryover under spelling-refs/), then hands the agent a
// staged dir and validates its output with the strongest validator in M5 - including
// a dry-run Apply+Check so a rule that would forge a name is rejected before it ever
// touches the real work dir.
func (e *Executor) spellingResearch(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	// 1) marker_titles.txt from the manifest (if an earlier stage did not write it).
	if err := ensureMarkerTitles(book.WorkDir); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: write marker_titles.txt: %w", err)
	}
	// 2) The chunk plan (compute + persist once; reuse a prior one on a re-run).
	plan, err := loadOrComputeChunkPlan(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: chunk plan: %w", err)
	}
	// 3) Series carryover: stage the predecessor's corrected texts + ledger under
	//    spelling-refs/ (the daemon populates it - the agent never reaches the other
	//    book's work dir).
	pred, hasCarryover, err := findSeriesPredecessor(ctx, e.db, book)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: find series predecessor: %w", err)
	}
	if hasCarryover {
		if err := populateSpellingRefs(book.WorkDir, pred.WorkDir); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: populate spelling-refs: %w", err)
		}
	}
	// 3b) The series glossary: the canonical names the community database already
	//     records for the OTHER volumes of this series. This is the only reference
	//     that can catch a name the whole book (and the whole carryover chain) gets
	//     consistently wrong - the transcript and the predecessor agree with each
	//     other precisely because the error propagated. Best-effort: an outage, a
	//     standalone work, or an uncontributed series yields no glossary and the
	//     stage proceeds exactly as before.
	glossary := e.seriesGlossary(ctx, book, r)

	// 4) Stage the agent inputs.
	st, err := agent.New(book.WorkDir, string(state.SpellingResearch), e.stageAttempt(ctx, book, state.SpellingResearch))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	textDir := filepath.Join(book.WorkDir, transcript.TextDir)
	if !isDir(textDir) {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: transcripts-text/ missing (sanitizing must run first)")
	}
	// Stage a COMPACT proper-noun-candidate report instead of the whole transcript.
	// spelling.ExtractCandidates distills the corrected/repaired layer (~600KB) into a
	// deterministic ~150KB candidate list, so the agent reads that rather than the
	// full text.
	cand, err := spelling.ExtractCandidates(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: extract candidates: %w", err)
	}
	// A non-trivial transcript that yields ZERO candidates means the extractor broke;
	// staging an empty shortlist would silently let the book finish with no corrections
	// and no signal. Fail loudly (a genuinely tiny book, below the floor, may proceed).
	if len(cand.Candidates) == 0 && cand.TotalWords > spellingCorpusFloor {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: candidate extraction produced zero candidates from a %d-word transcript - the extractor is likely broken; refusing to stage an empty shortlist", cand.TotalWords)
	}
	if cand.Truncated > 0 && r.Note != nil {
		r.Note(fmt.Sprintf("candidate report: %s staged, %s dropped to keep it compact (the highest-signal entries are kept)",
			countNoun(len(cand.Candidates), "candidate"), countNoun(cand.Truncated, "lower-signal candidate")))
	}
	candJSON, err := spelling.MarshalCandidates(cand)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: marshal candidates: %w", err)
	}
	if err := st.WriteFile(spelling.CandidatesFile, candJSON); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", spelling.CandidatesFile, err)
	}
	for _, name := range []string{audio.ManifestName, markerTitlesFile, chunkPlanFile} {
		if err := st.CopyFile(filepath.Join(book.WorkDir, name), name); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", name, err)
		}
	}
	// The deterministic reference pre-pass. The agent's own signal is intra-transcript
	// disagreement, and a name misheard the SAME way every time produces none - so
	// this compares the candidates against outside vocabularies and stages the
	// near-misses explicitly.
	refMatches := spelling.BuildReferenceMatches(cand, referenceSources(book.WorkDir, glossary))
	refJSON, err := spelling.MarshalReferenceMatches(refMatches)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: marshal reference matches: %w", err)
	}
	if err := st.WriteFile(spelling.ReferenceMatchesFile, refJSON); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", spelling.ReferenceMatchesFile, err)
	}
	if !refMatches.Empty() && r.Note != nil {
		r.Note(fmt.Sprintf("reference check: %s differ from a known spelling (%s)",
			countNoun(len(refMatches.Matches), "transcript form"), referenceMatchSummary(refMatches)))
	}
	// stageIfPresent keys off the file actually existing, so a glossary whose write
	// failed simply is not staged rather than failing the stage.
	glossaryRel := filepath.Join(spellingRefsDir, seriesGlossaryFile)
	if err := e.stageIfPresent(st, book.WorkDir, glossaryRel, glossaryRel); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", seriesGlossaryFile, err)
	}
	// Stage ONLY the small prior-* carryover files, never the predecessor's corrected
	// chapter texts (that would be another whole book of transcript in the context).
	// The full spelling-refs/ still lives in the work dir for the dry-run corpus and
	// the correcting stage; here we hand the agent just the ledger/rules/marker files.
	for _, ref := range priorRefNames {
		src := filepath.Join(book.WorkDir, spellingRefsDir, ref.dst)
		if !fsutil.IsFile(src) {
			continue // a missing staged file is skipped silently (best-effort carryover)
		}
		if err := st.CopyFile(src, filepath.Join(spellingRefsDir, ref.dst)); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", ref.dst, err)
		}
	}

	// WebAvailable reflects the resolved runner (claude/codex both support web today);
	// runAgent re-ensures and parks if none is available, so a nil runner here just
	// yields WebAvailable=false in the prompt.
	runner, _ := e.ensureAgent(ctx)
	data := spellingPromptData{
		Title:               book.Title,
		Authors:             authors(book),
		Series:              book.Series,
		SeriesPos:           book.SeriesPos,
		HasCarryover:        hasCarryover,
		WebAvailable:        runner != nil && runner.SupportsWeb(),
		ChunkEnds:           plan.chunkEndsCSV(),
		HasGlossary:         !glossary.Empty(),
		GlossaryWorks:       strings.Join(glossary.Works, ", "),
		HasReferenceMatches: !refMatches.Empty(),
		EvidencePriority:    evidencePriority(!glossary.Empty(), hasCarryover),
	}
	// Build the dry-run corpus (the immutable transcript layers + marker titles +
	// carryover refs) ONCE for the whole stage; each validation attempt reuses it and
	// only re-derives transcripts-corrected/. Cleaned up when the stage returns.
	dryRunDir, err := buildDryRunCorpus(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: build dry-run corpus: %w", err)
	}
	defer func() { _ = os.RemoveAll(dryRunDir) }()

	// Capture the validated rule/ledger counts from the successful attempt so no
	// post-harvest reload is needed for the metrics.
	var rules, ledgerEntries int
	validate := func(_ agent.Result, s *agent.Staging) error {
		r, l, verr := validateSpellingOutputs(s.OutDir(), dryRunDir, book.Title, plan)
		if verr != nil {
			return verr
		}
		rules, ledgerEntries = r, l
		return nil
	}
	usage, err := e.runAgent(ctx, book, state.SpellingResearch, r, st, "spelling.md", data, true, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	if err := agent.Harvest(st, []agent.HarvestSpec{
		{From: spelling.CorrectionsFile, To: spelling.CorrectionsFile},
		{From: spelling.SpellingsFile, To: spelling.SpellingsFile},
	}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: harvest: %w", err)
	}

	if r.Progress != nil {
		r.Progress(1, 1)
	}
	m := usage.metricsMap()
	m["rules"] = rules
	m["ledger_entries"] = ledgerEntries
	result := scheduler.StageResult{Metrics: metrics(m), RateSample: usage.rateSample()}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.SpellingResearch), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// ensureMarkerTitles writes marker_titles.txt (one line per manifest chapter: the
// marker title, falling back to the chapter title) if it is not already present.
func ensureMarkerTitles(workDir string) error {
	p := filepath.Join(workDir, markerTitlesFile)
	if fsutil.IsFile(p) {
		return nil
	}
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return fmt.Errorf("read manifest (inspect must run first): %w", err)
	}
	var b strings.Builder
	for _, ch := range m.Chapters {
		title := strings.TrimSpace(ch.MarkerTitle)
		if title == "" {
			title = strings.TrimSpace(ch.Title)
		}
		b.WriteString(title)
		b.WriteByte('\n')
	}
	return fsutil.WriteFileAtomic(p, []byte(b.String()), 0o644)
}

// populateSpellingRefs fills workDir/spelling-refs/ from the predecessor book: its
// corrected chapter texts plus its marker titles, ledger, and rules under prior-*
// names. A missing single-file source is skipped (best effort). The corrected texts
// feed the GATE-side attestation union only - the dry-run corpus and the correcting
// stage read them from the work dir so a rule may cite spelling-refs to attest a
// series name known from the predecessor's prose; the agent itself is never staged
// them (it gets only the small prior-* ledger/rules/marker files).
func populateSpellingRefs(workDir, predDir string) error {
	dst := filepath.Join(workDir, spellingRefsDir)
	// Already populated on a prior run: the predecessor's refs are immutable, so a
	// carryover that is already there needs no re-copy.
	//
	// The sentinel is THIS step's own output file, not "the directory is non-empty".
	// spelling-refs/ has more than one daemon-side writer now (seriesGlossary also
	// writes series-glossary.txt there), and a directory-level guard would let one
	// writer suppress another: a book whose first attempt ran before its series
	// predecessor finished would write a glossary, then find the directory non-empty
	// on the retry that finally has a predecessor and silently skip the carryover
	// entirely.
	if fsutil.IsFile(filepath.Join(dst, priorSpellingsFile)) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	corrDir := filepath.Join(predDir, spelling.CorrectedDir)
	if isDir(corrDir) {
		entries, err := os.ReadDir(corrDir)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".txt") {
				continue
			}
			if err := fsutil.CopyFile(filepath.Join(corrDir, ent.Name()), filepath.Join(dst, ent.Name()), 0o644); err != nil {
				return err
			}
		}
	}
	for _, ref := range priorRefNames {
		src := filepath.Join(predDir, ref.src)
		if !fsutil.IsFile(src) {
			continue
		}
		if err := fsutil.CopyFile(src, filepath.Join(dst, ref.dst), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// validateSpellingOutputs is the strongest M5 validator: it parses and .Validate()s
// both agent outputs, checks the chunk_ends match the plan exactly, the title matches
// the book, every ledger status is in the closed set, the reference_files are
// RESTRICTED to marker_titles.txt / spelling-refs (an agent may not attest its own
// inventions), and finally DRY-RUNS spelling.Apply + spelling.Check inside the
// pre-built dryRunDir corpus so a rule that fails a gate becomes retry feedback
// verbatim. It returns the validated rule + ledger counts for the stage metrics.
func validateSpellingOutputs(outDir, dryRunDir, title string, plan chunkPlan) (rules, ledger int, err error) {
	// LoadCorrections/LoadSpellings already run .Validate(), so a schema-invalid output
	// fails here without a separate .Validate() call.
	corr, err := spelling.LoadCorrections(outDir)
	if err != nil {
		return 0, 0, err
	}
	sp, err := spelling.LoadSpellings(outDir)
	if err != nil {
		return 0, 0, err
	}
	if !slices.Equal(sp.ChunkEnds, plan.ChunkEnds) {
		return 0, 0, fmt.Errorf("spellings chunk_ends %v must equal the chunk plan %v exactly", sp.ChunkEnds, plan.ChunkEnds)
	}
	if sp.Title != title {
		return 0, 0, fmt.Errorf("spellings title %q must equal the book title %q exactly", sp.Title, title)
	}
	for i, entry := range sp.Ledger {
		if !spellingLedgerStatuses[entry.Status] {
			return 0, 0, fmt.Errorf("ledger entry %d (%q) has status %q; every status must be verified, probable, or unresolved", i, entry.Canonical, entry.Status)
		}
	}
	if err := validateReferenceFiles(corr.ReferenceFiles); err != nil {
		return 0, 0, err
	}
	if err := dryRunCorrections(dryRunDir, corr); err != nil {
		return 0, 0, err
	}
	// The dry-run gates pass a rule that matches nothing (gate 1 wants zero LHS matches;
	// gates 2/3 pass when the RHS is attested elsewhere), so a mechanical dead-rule scan
	// against the original transcript layer catches the silent under-correction the
	// gates miss. Run it AFTER the gates so a genuine gate failure still surfaces first.
	if err := checkDeadRules(dryRunDir, corr); err != nil {
		return 0, 0, err
	}
	// Dry-run the spoiler-gated sheet generation against the corrected layer the dry-run
	// Apply just built. GenerateSheets' gate 1 requires every non-carryover ledger
	// canonical to OCCUR in the corrected layer - a ledger whose canonical is an external
	// spelling the uncorrected text does not contain (the agent left the name alone but
	// ledgered the verified form) fails it. Without this dry run that bad ledger passes
	// validation and kills the mechanical correcting stage instead. The audit is
	// EXHAUSTIVE (every gate + spoiler violation at once) so one patch retry can fix them
	// all, and adds two spoiler checks GenerateSheets does not perform (carryover
	// integrity + preamble safety); the prior-book ledger for the carryover check is the
	// one staged under spelling-refs/.
	priorSet, hasPrior, err := spelling.PriorCanonicals(filepath.Join(dryRunDir, spellingRefsDir, priorSpellingsFile))
	if err != nil {
		return 0, 0, fmt.Errorf("read staged prior-book ledger: %w", err)
	}
	if err := dryRunSheets(dryRunDir, sp, priorSet, hasPrior); err != nil {
		return 0, 0, err
	}
	return len(corr.Rules), len(sp.Ledger), nil
}

// dryRunSheets runs the EXHAUSTIVE validator-side sheet audit inside the pre-built
// corpus (whose transcripts-corrected/ the dry-run Apply just wrote), aggregating
// EVERY gate-1/gate-2 violation plus the carryover-integrity and preamble-safety
// checks into a single error so one patch retry can fix them all - the engine's
// GenerateSheets fails fast (one message per attempt), which drained the retry budget.
// After the audit is clean it still runs GenerateSheets as the authoritative confirm
// (the mechanical correcting stage runs it for real), so any drift between the
// exhaustive validator and the frozen engine surfaces here rather than parking
// correcting. facts/ under the throwaway corpus is disposable, so removing it keeps
// attempts independent exactly like dryRunCorrections does for transcripts-corrected/.
func dryRunSheets(dryRunDir string, sp *spelling.Spellings, priorSet map[string]bool, hasPrior bool) error {
	violations, err := spelling.AuditSheets(dryRunDir, sp, priorSet, hasPrior)
	if err != nil {
		return fmt.Errorf("dry-run spelling-sheet validation failed: %v", err)
	}
	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("the spelling sheets have spoiler/gate problems - fix ALL of them in one patch pass:")
		for _, v := range violations {
			b.WriteString("\n- ")
			b.WriteString(v)
		}
		return errors.New(b.String())
	}
	if err := os.RemoveAll(filepath.Join(dryRunDir, spelling.FactsDir)); err != nil {
		return err
	}
	if _, err := spelling.GenerateSheets(dryRunDir, sp); err != nil {
		return fmt.Errorf("dry-run spelling-sheet generation failed: %v", err)
	}
	return nil
}

// checkDeadRules rejects any correction rule whose pattern matches nothing in the
// original transcript layer - a silent no-op the four Check gates do not catch. The
// message names EACH dead pattern verbatim so it rides into the agent's retry prompt.
func checkDeadRules(dryRunDir string, corr *spelling.Corrections) error {
	dead, err := spelling.DeadRules(dryRunDir, corr)
	if err != nil {
		return fmt.Errorf("dead-rule check failed: %v", err)
	}
	if len(dead) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("one or more correction rules are dead (their pattern matches nothing in the transcript):")
	for _, r := range dead {
		fmt.Fprintf(&b, "\nrule pattern %q matches nothing in the transcript - a dead rule; delete it or fix its pattern to a form that actually occurs", r.Pattern)
	}
	return errors.New(b.String())
}

// seriesGlossary fetches this book's series glossary from the community metadata
// database and writes it under spelling-refs/. It is best-effort at every step: no
// metadata client, no work id, an unreachable service, a standalone work, or a
// series whose other volumes carry no sidecars all yield an empty glossary. The
// stage then runs exactly as it did before, so this can never park a book.
func (e *Executor) seriesGlossary(ctx context.Context, book store.Book, r scheduler.StageReport) metaops.Glossary {
	path := filepath.Join(book.WorkDir, spellingRefsDir, seriesGlossaryFile)
	// A run that produces no glossary must leave none behind. The work dir is
	// durable, so a file from an earlier successful run would otherwise still be
	// staged to the agent (stageIfPresent keys on the file) and still sit in the
	// correction gate's attestation corpus, while HasGlossary is false and the
	// pre-pass has no glossary vocabulary - the prompt would describe no such file
	// and the agent would find one. Worse, book.WorkID is mutable: a book re-matched
	// to a DIFFERENT work would keep the previous series' canonical names on disk,
	// where they could attest a rule renaming a character to a wrong-series name.
	discard := func() metaops.Glossary {
		_ = os.Remove(path)
		return metaops.Glossary{}
	}

	workID := strings.TrimSpace(book.WorkID)
	if workID == "" || e.meta == nil {
		return discard()
	}
	g, err := e.meta.SeriesGlossary(ctx, workID)
	if err != nil || g.Empty() {
		return discard()
	}
	// Atomic: this file becomes the agent's highest-authority spelling evidence AND
	// rides into the correction gate's attestation corpus, so a torn write from a
	// crash must never be picked up by the next run. WriteFileAtomic MkdirAlls the
	// parent, so no explicit MkdirAll is needed here.
	if err := fsutil.WriteFileAtomic(path, []byte(g.Lines()), 0o644); err != nil {
		return discard()
	}
	if r.Note != nil {
		note := fmt.Sprintf("series glossary: %s from %s in %q",
			countNoun(len(g.Names), "canonical name"), countNoun(len(g.Works), "sibling volume"), g.SeriesName)
		if g.Truncated > 0 {
			note += fmt.Sprintf(" (%s dropped to keep it compact)", countNoun(g.Truncated, "further name"))
		}
		r.Note(note)
	}
	return g
}

// evidencePriority renders spelling.md's ordered evidence list.
//
// It is composed here rather than in the template because two of the seven tiers
// are conditional, and expressing that with nested {{if}} means writing the whole
// hand-numbered list once per combination - four copies today, eight the next time
// a staged source is added, each free to drift from the others. The repo already
// composes prompt blocks in Go for the same reason (EdgeNote, ChunkNote,
// AssembleNote).
//
// The first two tiers are the daemon-staged vocabularies, ranked exactly as
// spelling.Authority ranks them; the tail is the outside evidence only the agent
// can reach.
func evidencePriority(hasGlossary, hasCarryover bool) string {
	var items []string
	if hasGlossary {
		items = append(items, "the community series glossary (`spelling-refs/series-glossary.txt`)")
	}
	items = append(items, "embedded metadata and exact chapter-marker labels (`marker_titles.txt`)")
	if hasCarryover {
		items = append(items, "the carried series ledger (`spelling-refs/prior-spellings.json`)")
	}
	items = append(items,
		"official author, publisher, or series material",
		"the book's catalogue records or official table of contents",
		"book-scoped wiki page TITLES or structured navigation",
		"agreement among multiple independent references",
	)
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = fmt.Sprintf("%d. %s", i+1, it)
	}
	return strings.Join(lines, "\n")
}

// referenceSources assembles the vocabularies BuildReferenceMatches compares the
// transcript against, in descending authority.
//
// The ranking is the point. The series glossary and the publisher's chapter titles
// are VERIFIED: they come from outside this book's audio, so they can contradict it.
// The predecessor's ledger is CARRYOVER: it is evidence that the previous volume
// spelled a name the same way, which is not evidence that the spelling is right -
// one misheard name reproduces itself down a whole series precisely because every
// book agrees with the one before it.
func referenceSources(workDir string, glossary metaops.Glossary) []spelling.ReferenceSource {
	var out []spelling.ReferenceSource
	add := func(name string, auth spelling.Authority, names []string) {
		if len(names) > 0 {
			out = append(out, spelling.ReferenceSource{Name: name, Authority: auth, Names: names})
		}
	}

	add(filepath.Join(spellingRefsDir, seriesGlossaryFile), spelling.AuthorityVerified, glossary.Names)

	// The publisher's own chapter titles. Rich for some books ("Chapter 9: Toren"),
	// a bare number table for others - in which case this contributes nothing, which
	// is exactly why it cannot be the only reference.
	if b, err := os.ReadFile(filepath.Join(workDir, markerTitlesFile)); err == nil { //nolint:gosec // daemon-written path
		add(markerTitlesFile, spelling.AuthorityVerified, spelling.NamesFromTitles(string(b)))
	}

	add(filepath.Join(spellingRefsDir, priorSpellingsFile), spelling.AuthorityCarryover, priorLedgerNames(workDir))
	return out
}

// priorLedgerNames reads the canonical forms out of the series predecessor's staged
// ledger, sorted so equidistant matches resolve deterministically. It reuses
// spelling.PriorCanonicals (the same reader the sheet audit uses on this file), so
// the ledger's wire shape stays mirrored in exactly one place. A missing or
// unreadable ledger yields no names - the carryover is best-effort.
func priorLedgerNames(workDir string) []string {
	set, ok, err := spelling.PriorCanonicals(filepath.Join(workDir, spellingRefsDir, priorSpellingsFile))
	if err != nil || !ok {
		return nil
	}
	return slices.Sorted(maps.Keys(set))
}

// referenceMatchSummary renders the highest-signal proposals for the stage note, so
// the book log shows what the pre-pass actually found rather than just a count.
func referenceMatchSummary(r *spelling.ReferenceMatches) string {
	const maxShown = 3
	parts := make([]string, 0, maxShown)
	for i, m := range r.Matches {
		if i >= maxShown {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("%s -> %s", m.Form, m.Reference))
	}
	return strings.Join(parts, ", ")
}

// validateReferenceFiles enforces the gate-3 integrity boundary: a rule may only be
// attested against marker_titles.txt or a file under spelling-refs/ (the daemon-staged
// carryover). Anything else - especially an agent-authored file - is rejected so an
// invented name cannot attest itself.
func validateReferenceFiles(refs []string) error {
	for _, ref := range refs {
		if !allowedReferenceFile(ref) {
			return fmt.Errorf("reference_files entry %q is not allowed - only %q and files under %q/ may be cited (an agent must not attest names against its own output)", ref, markerTitlesFile, spellingRefsDir)
		}
	}
	return nil
}

// allowedReferenceFile reports whether a reference_files entry is within the allowed
// set: exactly marker_titles.txt, exactly spelling-refs, or a non-traversing path
// under spelling-refs/.
func allowedReferenceFile(ref string) bool {
	r := strings.TrimSpace(ref)
	if r == "" || filepath.IsAbs(r) {
		return false
	}
	clean := filepath.Clean(r)
	if clean == markerTitlesFile || clean == spellingRefsDir {
		return true
	}
	return strings.HasPrefix(clean, spellingRefsDir+string(os.PathSeparator))
}

// buildDryRunCorpus copies the immutable correction inputs (both transcript layers, the
// marker titles, and the carryover refs) into a throwaway temp dir ONCE per stage run.
// dryRunCorrections re-runs Apply+Check against it per validation attempt, re-deriving
// only transcripts-corrected/; the caller removes the dir when the stage returns. It
// never touches the real work dir.
func buildDryRunCorpus(workDir string) (string, error) {
	tmp, err := os.MkdirTemp("", "spelling-dryrun-*")
	if err != nil {
		return "", err
	}
	for _, d := range []string{transcript.TextDir, transcript.RepairedDir, spellingRefsDir} {
		src := filepath.Join(workDir, d)
		if !isDir(src) {
			continue
		}
		if err := copyDirPlain(src, filepath.Join(tmp, d)); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	if src := filepath.Join(workDir, markerTitlesFile); fsutil.IsFile(src) {
		if err := fsutil.CopyFile(src, filepath.Join(tmp, markerTitlesFile), 0o644); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	return tmp, nil
}

// dryRunCorrections runs spelling.Apply then spelling.Check inside the pre-built corpus
// dir and returns any gate failure verbatim (so it rides into the agent's retry
// prompt). It removes a prior attempt's transcripts-corrected/ output first (the
// immutable sources stay), so the corpus is reused across attempts without a rebuild.
func dryRunCorrections(dryRunDir string, corr *spelling.Corrections) error {
	if err := os.RemoveAll(filepath.Join(dryRunDir, spelling.CorrectedDir)); err != nil {
		return err
	}
	if _, err := spelling.Apply(dryRunDir, corr); err != nil {
		return fmt.Errorf("dry-run apply of the corrections failed: %v", err)
	}
	res, err := spelling.Check(dryRunDir, corr)
	if err != nil {
		return fmt.Errorf("dry-run check of the corrections failed: %v", err)
	}
	if !res.Ok() {
		return fmt.Errorf("the corrections fail the spelling gates:\n%s", res.Summary())
	}
	return nil
}

// --- correcting (Lane C, MECHANICAL) ---

// correcting applies the researched corrections to build the corrected transcript
// layer, verifies it against the four gates (a failure parks - it should be rare given
// spelling_research's dry run), and generates the spoiler-gated per-chunk spelling
// sheets. It is mechanical: no agent.
func (e *Executor) correcting(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if err := ctx.Err(); err != nil {
		return scheduler.StageResult{}, err
	}
	corr, err := spelling.LoadCorrections(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("correcting: load corrections (spelling_research must run first): %w", err)
	}
	sp, err := spelling.LoadSpellings(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("correcting: load spellings (spelling_research must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("applying %s to the transcripts", countNoun(len(corr.Rules), "correction rule")))
	}
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	start := time.Now()
	applyRes, err := spelling.Apply(book.WorkDir, corr)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("correcting: apply corrections: %w", err)
	}
	checkRes, err := spelling.Check(book.WorkDir, corr)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("correcting: check corrections: %w", err)
	}
	if !checkRes.Ok() {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkSpellingGateFailure, SpellingGateFailurePrefix+":\n"+checkRes.Summary())
	}
	sheetsRes, err := spelling.GenerateSheets(book.WorkDir, sp)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("correcting: generate spelling sheets: %w", err)
	}
	correctSeconds := time.Since(start).Seconds()
	// NOTE: spelling.CheckFirstUse (the first-use-before-attestation cross-check) is
	// deliberately NOT wired here. It needs the fact-pass ROSTER, which does not exist
	// yet at correcting time (correcting runs before fact_pass), so every call landed in
	// "skipped" and produced constant-zero firstuse_* metrics while paying a full
	// corrected-corpus read per sheet. The engine itself (spelling.CheckFirstUse) is
	// real and tested.
	// TODO(M5+): run CheckFirstUse from the auditing stage against the fact-pass roster,
	// where the roster the check needs is available.

	e.accountScratch(ctx, book)
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"chapters":     applyRes.Chapters,
			"replacements": applyRes.Replacements,
			"rules_fired":  applyRes.RulesFired,
			"sheets":       len(sheetsRes.Sheets),
		}),
		RateSample: rateSample(1, correctSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Correcting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// --- shared plain filesystem helpers (no staging semantics) ---

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// copyDirPlain copies every regular file under srcDir into dstDir (0644, preserving
// the sub-tree) via the shared fsutil.CopyFile primitive.
func copyDirPlain(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		return fsutil.CopyFile(path, filepath.Join(dstDir, rel), 0o644)
	})
}
