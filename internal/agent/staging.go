package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// DefaultHarvestMaxBytes bounds a single harvested output file (2 MiB). A stage may
// raise it per spec, but the default protects the work dir from a runaway agent.
const DefaultHarvestMaxBytes = 2 << 20

// Staged-dir layout names, exported so pipeline stages can address the same paths
// without a Staging handle. A per-attempt staged dir lives at
// <workDir>/<RunsDir>/<stage>-a<NN>/ and the agent writes into its <OutDirName>/.
const (
	RunsDir    = "_runs"
	OutDirName = "out"
)

// OutPath returns the agent's output directory for a staged request dir (the cwd an
// agent runs against). It is the inverse of the layout Staging.OutDir builds.
func OutPath(reqDir string) string { return filepath.Join(reqDir, OutDirName) }

// inputPerm is applied to every staged INPUT file so the agent cannot mutate its
// own inputs mid-run; out/ stays writable for the agent's outputs.
const inputPerm = 0o444

// Staging builds the per-attempt context directory an agent runs against:
// <workDir>/_runs/<stage>-a<NN>/, holding ONLY the stage's declared inputs (copied,
// never symlinked, read-only) plus an out/ dir the agent writes into. Retained after
// the run for debuggability; PurgeScratch removes _runs wholesale.
type Staging struct {
	workDir string
	dir     string
	out     string
}

// New creates a fresh staged dir for (stage, attempt). It removes any leftover dir
// from a prior attempt with the same name so a re-run starts clean.
func New(workDir, stage string, attempt int) (*Staging, error) {
	name := fmt.Sprintf("%s-a%02d", stage, attempt)
	dir := filepath.Join(workDir, RunsDir, name)
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clean staged dir: %w", err)
	}
	out := filepath.Join(dir, OutDirName)
	// 0750: the agent child runs as this same user, so owner rwx is all it needs to
	// write outputs (matches the asr OutDir convention and the lint baseline).
	if err := os.MkdirAll(out, 0o750); err != nil {
		return nil, fmt.Errorf("create staged dir: %w", err)
	}
	return &Staging{workDir: workDir, dir: dir, out: out}, nil
}

// Dir is the staged directory (the agent's cwd).
func (s *Staging) Dir() string { return s.dir }

// OutDir is the directory the agent must write its outputs into.
func (s *Staging) OutDir() string { return s.out }

// CopyFile copies src to relDst under the staged dir (read-only). relDst is
// validated to stay inside the staged dir (no absolute, no traversal).
func (s *Staging) CopyFile(src, relDst string) error {
	dst, err := safeJoin(s.dir, relDst)
	if err != nil {
		return err
	}
	return fsutil.CopyFile(src, dst, inputPerm)
}

// CopyDir copies every regular file under srcDir into relDstDir under the staged
// dir, preserving the relative sub-tree, read-only. When filter is non-nil, only
// files whose path relative to srcDir satisfies filter(rel) are copied.
func (s *Staging) CopyDir(srcDir, relDstDir string, filter func(rel string) bool) error {
	dstRoot, err := safeJoin(s.dir, relDstDir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		if filter != nil && !filter(rel) {
			return nil
		}
		return fsutil.CopyFile(path, filepath.Join(dstRoot, rel), inputPerm)
	})
}

// WriteFile writes data to relDst under the staged dir (read-only input). relDst is
// validated to stay inside the staged dir.
func (s *Staging) WriteFile(relDst string, data []byte) error {
	dst, err := safeJoin(s.dir, relDst)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(dst, data, inputPerm)
}

// HarvestSpec declares one output to move from the staged out/ into the work dir.
type HarvestSpec struct {
	From     string // rel path within out/
	To       string // rel path within the work dir
	MaxBytes int64  // 0 = DefaultHarvestMaxBytes
}

// Harvest copies each declared output from out/ into the work dir. It is
// security-critical: From must resolve inside out/ and To inside the work dir (no
// absolute, no traversal), the resolved source must not escape out/ via a symlink,
// and each file is size-capped. Harvested files land 0644 (normal artifacts).
func Harvest(s *Staging, specs []HarvestSpec) error {
	outReal, err := filepath.EvalSymlinks(s.out)
	if err != nil {
		return fmt.Errorf("resolve out dir: %w", err)
	}
	for _, spec := range specs {
		src, err := safeJoin(s.out, spec.From)
		if err != nil {
			return fmt.Errorf("harvest %q: %w", spec.From, err)
		}
		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("harvest %q: %w", spec.From, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("harvest %q: refusing symlink in out/", spec.From)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("harvest %q: not a regular file", spec.From)
		}
		// Reject any component symlink that redirects the resolved path outside out/.
		real, err := filepath.EvalSymlinks(src)
		if err != nil {
			return fmt.Errorf("harvest %q: %w", spec.From, err)
		}
		if !fsutil.Within(outReal, real) {
			return fmt.Errorf("harvest %q: resolved path escapes out/", spec.From)
		}
		max := spec.MaxBytes
		if max <= 0 {
			max = DefaultHarvestMaxBytes
		}
		if info.Size() > max {
			return fmt.Errorf("harvest %q: %d bytes exceeds cap %d", spec.From, info.Size(), max)
		}
		dst, err := safeJoin(s.workDir, spec.To)
		if err != nil {
			return fmt.Errorf("harvest %q -> %q: %w", spec.From, spec.To, err)
		}
		data, err := os.ReadFile(src) //nolint:gosec // src is validated inside the staged out/ dir
		if err != nil {
			return fmt.Errorf("harvest %q: %w", spec.From, err)
		}
		if err := fsutil.WriteFileAtomic(dst, data, 0o644); err != nil {
			return fmt.Errorf("harvest %q -> %q: %w", spec.From, spec.To, err)
		}
	}
	return nil
}

// safeJoin cleans rel and joins it under root, rejecting an absolute path, an empty
// path, or one that escapes root via "..". The returned path is guaranteed to sit
// inside root.
func safeJoin(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed: %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || clean == "." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	joined := filepath.Join(root, clean)
	if !fsutil.Within(root, joined) {
		return "", fmt.Errorf("path escapes root: %q", rel)
	}
	return joined, nil
}
