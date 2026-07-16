// Package fsutil holds the tiny filesystem helpers shared across the daemon's
// artifact writers. It has no dependencies beyond the standard library and knows
// nothing about the pipeline, so any package (audio, transcript, pipeline, asr)
// can depend on it as a leaf.
package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
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
