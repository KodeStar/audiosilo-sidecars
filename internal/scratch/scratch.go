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

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
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
	if wd == root || !within(wd, root) {
		return "", false
	}
	return wd, true
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel) && !filepath.IsAbs(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && (rel[2] == filepath.Separator)
}

// Purge deletes a book's reclaimable scratch - the split chapters/ directory (and,
// once M-later copies sources locally, the copied source) - while KEEPING the
// durables (probe.json, manifest.json, transcripts, facts, sidecars). It is a
// no-op when the work dir is absent. The deletion is confined to workRoot.
func Purge(workRoot, workDir string) error {
	wd, ok := Confined(workRoot, workDir)
	if !ok {
		return nil // nothing safe to remove
	}
	return os.RemoveAll(filepath.Join(wd, audio.ChaptersDir))
}
