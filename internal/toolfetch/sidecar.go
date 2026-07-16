package toolfetch

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// This file holds the shared .meta sidecar plumbing: a downloaded artifact (an ASR
// model, the whisper-cli distribution) carries a small JSON sidecar beside it that
// a later cache-hit check reads to decide whether the cached bytes are still
// trustworthy. metaPath (model.go) derives the sidecar location.

// readJSONSidecar reads and parses a JSON sidecar into T. ok=false when the file is
// absent or unparseable, so the caller treats the cache it describes as needing a
// (re)download rather than trusting it.
func readJSONSidecar[T any](path string) (T, bool) {
	var zero T
	data, err := os.ReadFile(path) //nolint:gosec // path is our own <artifact>.meta under the tools dir
	if err != nil {
		return zero, false
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return zero, false
	}
	return v, true
}

// writeJSONSidecar marshals v and writes it via temp + rename, so a crash mid-write
// leaves either the old sidecar or none - never a torn file a cache-hit check could
// misread. Callers write the sidecar LAST, after the artifact it vouches for is
// fully in place.
func writeJSONSidecar(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
