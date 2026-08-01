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

// reclaimable names the work-dir subdirectories a purge removes: the split
// chapters/, the agent staged-context dirs under _runs/, and the M5 tail-clip /
// full-chapter re-transcription scratch (clips/, retranscribe/). None is a durable
// (transcripts, facts, sidecars, reports) and none is a stage INPUT (so removing them
// invalidates no sentinel), so a purge can reclaim them freely.
var reclaimable = []string{audio.ChaptersDir, agent.RunsDir, repair.ClipsDir, repair.RetranscribeDir}

// ebookReclaimable names the layers an EBOOK book reclaims on top of the shared
// list: the raw split output and the per-chapter text derived from it.
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
var ebookReclaimable = []string{ebook.ExtractDir, ebook.TextDir}

// Purge deletes a book's reclaimable scratch - the split chapters/ directory, the
// agent staged-context dirs (_runs/), and the tail-clip/re-transcription scratch
// (clips/, retranscribe/) - while KEEPING the durables (probe.json, manifest.json,
// transcripts, facts, sidecars). It is a no-op when the work dir is absent. The
// deletion is confined to workRoot.
//
// kind selects the extra ebook layers (see ebookReclaimable); an empty or unknown
// kind is treated as audio, so a pre-migration book reclaims exactly what it always
// did.
func Purge(workRoot, workDir string, kind state.Kind) error {
	wd, ok := Confined(workRoot, workDir)
	if !ok {
		return nil // nothing safe to remove
	}
	dirs := reclaimable
	if kind == state.KindEbook {
		dirs = slices.Concat(reclaimable, ebookReclaimable)
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(filepath.Join(wd, dir)); err != nil {
			return err
		}
	}
	return nil
}
