package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
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

// spellingLedgerStatuses is the closed set of ledger statuses the validator accepts.
var spellingLedgerStatuses = map[string]bool{"verified": true, "probable": true, "unresolved": true}

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

	// 4) Stage the agent inputs.
	st, err := agent.New(book.WorkDir, string(state.SpellingResearch), e.stageAttempt(ctx, book, state.SpellingResearch))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	textDir := filepath.Join(book.WorkDir, transcript.TextDir)
	if !isDir(textDir) {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: transcripts-text/ missing (sanitizing must run first)")
	}
	if err := st.CopyDir(textDir, transcript.TextDir, nil); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage transcripts-text: %w", err)
	}
	if repDir := filepath.Join(book.WorkDir, transcript.RepairedDir); isDir(repDir) {
		if err := st.CopyDir(repDir, transcript.RepairedDir, nil); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage transcripts-repaired: %w", err)
		}
	}
	for _, name := range []string{audio.ManifestName, markerTitlesFile, chunkPlanFile} {
		if err := st.CopyFile(filepath.Join(book.WorkDir, name), name); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage %s: %w", name, err)
		}
	}
	if refsDir := filepath.Join(book.WorkDir, spellingRefsDir); isDir(refsDir) {
		if err := st.CopyDir(refsDir, spellingRefsDir, nil); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("spelling_research: stage spelling-refs: %w", err)
		}
	}

	// WebAvailable reflects the resolved runner (claude/codex both support web today);
	// runAgent re-ensures and parks if none is available, so a nil runner here just
	// yields WebAvailable=false in the prompt.
	runner, _ := e.ensureAgent(ctx)
	data := spellingPromptData{
		Title:        book.Title,
		Authors:      authors(book),
		Series:       book.Series,
		SeriesPos:    book.SeriesPos,
		HasCarryover: hasCarryover,
		WebAvailable: runner != nil && runner.SupportsWeb(),
		ChunkEnds:    plan.chunkEndsCSV(),
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
// corrected chapter texts (the carryover attestation corpus) plus its marker titles,
// ledger, and rules under prior-* names. A missing single-file source is skipped
// (best effort); the corrected texts are the load-bearing attestation set.
func populateSpellingRefs(workDir, predDir string) error {
	dst := filepath.Join(workDir, spellingRefsDir)
	// Already populated on a prior run: the predecessor's refs are immutable, so a
	// non-empty spelling-refs/ needs no re-copy.
	if dirNonEmpty(dst) {
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
	for _, m := range []struct{ src, dst string }{
		{markerTitlesFile, "prior-marker_titles.txt"},
		{spelling.SpellingsFile, "prior-spellings.json"},
		{spelling.CorrectionsFile, "prior-corrections.json"},
	} {
		src := filepath.Join(predDir, m.src)
		if !fsutil.IsFile(src) {
			continue
		}
		if err := fsutil.CopyFile(src, filepath.Join(dst, m.dst), 0o644); err != nil {
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
	return len(corr.Rules), len(sp.Ledger), nil
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

// dirNonEmpty reports whether path is a directory holding at least one entry.
func dirNonEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
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
