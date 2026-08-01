// Package scratch tracks and reclaims the on-disk scratch a book's work dir
// accumulates. In M2 the heavy artifacts are the split chapter FLACs (and, later,
// a copied source); the durables (transcripts, facts, sidecars) are kept. It
// exposes disk-usage gauges (per book and daemon-total) and a manual purge of the
// reclaimable artifacts. Auto-purge and startup GC arrive in M7; for now purge is
// user-initiated from the UI.
//
// Every deletion is confined to the daemon's work root by Confined, mirroring the
// scheduler's Delete guard, so a doctored or legacy WorkDir can never make a purge
// remove an arbitrary path.
package scratch

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// DirSize returns the total size in bytes of the regular files under path
// (recursive). A missing path is 0 bytes with no error - a book whose work dir was
// never created, or already purged, simply reports zero. Other walk errors on
// individual entries are skipped so a transient unreadable file never fails the
// whole gauge.
func DirSize(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries rather than fail the gauge
		}
		if d.IsDir() {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

// Confined resolves workDir and reports it (absolute) only when it lives strictly
// inside workRoot. It returns ok=false for an empty root/dir, an unresolvable
// path, or a path at or outside the root - the caller must not touch the
// filesystem when ok is false. This is the shared guard for every destructive
// scratch operation.
func Confined(workRoot, workDir string) (string, bool) {
	if workRoot == "" || workDir == "" {
		return "", false
	}
	root, err := filepath.Abs(workRoot)
	if err != nil {
		return "", false
	}
	wd, err := filepath.Abs(workDir)
	if err != nil {
		return "", false
	}
	if wd == root || !fsutil.Within(root, wd) {
		return "", false
	}
	return wd, true
}

// artifact is one reclaimable work-dir directory together with the stage that
// produced it.
//
// The pairing is the point. A purge must remove the directory AND drop the sentinel
// of the stage that wrote it, or runStage's crash-resume fast path skips that stage
// on the next Retry and hands its successor an empty directory. Keeping the two
// facts in one table is what stops them drifting: they were previously two lists in
// two packages, and the ebook one had already lost chapter_mapping - a book parked
// at chapter_mapping, purged, then retried re-ran extracting (which writes no text
// for a non-contiguous universe), skipped chapter_mapping on its surviving sentinel,
// and wedged permanently at fact_pass with no chunk plan.
//
// Stage may be empty for a directory no stage's resume depends on.
type artifact struct {
	Dir   string
	Stage state.State
}

// audioArtifacts are reclaimed for every book: the split chapters/, the agent
// staged-context dirs under _runs/, and the tail-clip / full-chapter
// re-transcription scratch. Only chapters/ is a stage INPUT, so only splitting has a
// sentinel to invalidate.
var audioArtifacts = []artifact{
	{Dir: audio.ChaptersDir, Stage: state.Splitting},
	{Dir: agent.RunsDir},
	{Dir: repair.ClipsDir},
	{Dir: repair.RetranscribeDir},
}

// ebookArtifacts are reclaimed on top of the shared list for an EBOOK book: the raw
// split output and the per-chapter text derived from it.
//
// They are kind-specific rather than global because the audio path's equivalent
// layer, transcripts-corrected/, is DURABLE - it is the n-gram source for a later
// re-validation and the corpus a sequel's spelling carryover copies from. Adding it
// to the shared list would delete an audio book's corrected transcripts.
//
// For an ebook the opposite holds, and not only for disk: this IS the book's
// copyrighted source prose. The repo's rule is that source material never outlives
// the derivation, so purging it is required, not an optimization - and it is cheap
// to regenerate from the epub if a retry needs it.
//
// BOTH stages are listed against the text layer because either can write it:
// extracting materializes it only when the derived universe is contiguous, and
// chapter_mapping writes it otherwise.
var ebookArtifacts = []artifact{
	{Dir: ebook.ExtractDir, Stage: state.Extracting},
	{Dir: ebook.TextDir, Stage: state.ChapterMapping},
}

// artifactsFor returns the artifacts a book of this kind reclaims. An empty or
// unknown kind is audio, so a pre-migration book reclaims exactly what it always did.
func artifactsFor(kind state.Kind) []artifact {
	if kind == state.KindEbook {
		return slices.Concat(audioArtifacts, ebookArtifacts)
	}
	return audioArtifacts
}

// InvalidatedStages returns the stages whose sentinels a purge must drop for this
// kind, derived from the same table Purge deletes from so the two cannot disagree.
func InvalidatedStages(kind state.Kind) []state.State {
	var out []state.State
	for _, a := range artifactsFor(kind) {
		if a.Stage != "" && !slices.Contains(out, a.Stage) {
			out = append(out, a.Stage)
		}
	}
	return out
}

// PurgeRequired reports whether reclaiming this kind's scratch is an obligation
// rather than a disk-space preference.
//
// An ebook's reclaimed layers hold the book's copyrighted source prose, and "source
// material never outlives the derivation" is a licensing rule - so it must not be
// switchable off by contribution.auto_purge, which exists to control disk use. The
// predicate lives here, beside the artifact list that makes it true, rather than
// being restated as a kind comparison at each scheduler call site.
func PurgeRequired(kind state.Kind) bool {
	return kind == state.KindEbook
}

// HasReclaimable reports whether any reclaimable directory is actually present.
//
// It is the cheap precondition for a purge: a few stats against a full RemoveAll
// sweep, a recursive DirSize walk of the whole work dir and a DB write. The
// scratch_bytes gauge cannot serve that purpose, because DirSize counts the DURABLES
// too - so an already-purged book still reports a non-zero size and would be swept
// again on every daemon boot.
func HasReclaimable(workRoot, workDir string, kind state.Kind) bool {
	wd, ok := Confined(workRoot, workDir)
	if !ok {
		return false
	}
	for _, a := range artifactsFor(kind) {
		if _, err := os.Stat(filepath.Join(wd, a.Dir)); err == nil {
			return true
		}
	}
	return false
}

// Purge deletes a book's reclaimable scratch - the split chapters/ directory, the
// agent staged-context dirs (_runs/), and the tail-clip/re-transcription scratch
// (clips/, retranscribe/) - while KEEPING the durables (probe.json, manifest.json,
// transcripts, facts, sidecars). It is a no-op when the work dir is absent. The
// deletion is confined to workRoot.
//
// kind selects the extra ebook layers (see ebookArtifacts); an empty or unknown kind
// is treated as audio, so a pre-migration book reclaims exactly what it always did.
func Purge(workRoot, workDir string, kind state.Kind) error {
	wd, ok := Confined(workRoot, workDir)
	if !ok {
		return nil // nothing safe to remove
	}
	for _, a := range artifactsFor(kind) {
		if err := os.RemoveAll(filepath.Join(wd, a.Dir)); err != nil {
			return err
		}
	}
	return nil
}
