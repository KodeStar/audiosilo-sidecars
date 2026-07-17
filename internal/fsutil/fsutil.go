// Package fsutil holds the tiny filesystem helpers shared across the daemon's
// artifact writers. It has no dependencies beyond the standard library and knows
// nothing about the pipeline, so any package (audio, transcript, pipeline, asr)
// can depend on it as a leaf.
package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WriteFileAtomic writes data to path via a sibling temp file + rename, creating
// the parent directory if absent, so a crash never leaves a half-written artifact
// that a later run would trust. perm is applied to the final file explicitly (a
// Chmod on the temp file), so the result is deterministic regardless of the
// process umask. An existing file at path is atomically replaced.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpName)
		return cerr
	}
	if cerr := os.Chmod(tmpName, perm); cerr != nil {
		_ = os.Remove(tmpName)
		return cerr
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		_ = os.Remove(tmpName)
		return rerr
	}
	return nil
}

// IsFile reports whether path exists and is a regular file (not a directory).
func IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// CopyFile copies src to dst atomically (via WriteFileAtomic's temp+rename), creating
// the parent directory if absent and applying perm to the result. It is the single
// shared file-copy primitive so callers do not each reimplement temp+rename. src is
// read fully into memory - callers copy small artifacts (staged inputs, per-chapter
// text, carryover refs), not large media.
func CopyFile(src, dst string, perm fs.FileMode) error {
	data, err := os.ReadFile(src) //nolint:gosec // src derives from a trusted work/data dir
	if err != nil {
		return err
	}
	return WriteFileAtomic(dst, data, perm)
}

// Within reports whether path is root itself or lies inside it, lexically (no symlink
// resolution - callers that care resolve first). It rejects a path that escapes root
// via ".." and a filepath.Rel result that is absolute (the cross-volume edge where
// path shares no common base with root), so it is safe as a containment guard.
func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
