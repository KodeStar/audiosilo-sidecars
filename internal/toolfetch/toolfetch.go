// Package toolfetch resolves and, when necessary, fetches the external artifacts
// the pipeline needs. It manages three artifact families, all cached under
// <data>/tools:
//
//   - ffmpeg/ffprobe static builds (this file): Resolve owns the full lookup
//     order - an explicit configured path, then a copy next to the daemon binary,
//     then $PATH - and only if none of those turn up a tool (and auto-download is
//     enabled) does it fall back to ensure, which caches a static build and
//     reuses it forever. Ported from audiosilo-server's internal/toolfetch.
//   - the whisper.cpp `whisper-cli` binary (whisper.go): EnsureWhisperCLI fetches
//     the platform+device build from this repo's pinned release, verified against
//     the release's checksums.txt (a missing line or a digest mismatch adopts
//     nothing), with a one-shot CPU fallback when an accelerated build fails its
//     self-check on the user's machine and a stale-cache degrade when a refresh
//     fails offline.
//   - ggml ASR models (model.go): EnsureModel downloads a model with a size floor
//     and a .meta sidecar, so a truncated/corrupted cache is re-fetched rather
//     than trusted forever.
//
// Everything degrades gracefully: offline, auto-download disabled, or an
// unsupported platform just means the artifact is absent (the affected stage then
// parks/fails that book while the rest of the daemon keeps working) and a retry
// on the next run. Integrity: downloads are HTTPS-only from pinned, reputable
// hosts (BtbN's FFmpeg-Builds for Linux/Windows and evermeet.cx for macOS ffmpeg;
// this repo's GitHub release for whisper-cli; Hugging Face for models), and every
// downloaded binary is sanity-checked by running it (`-version` / `--help`)
// before it is adopted. Extraction is fully in-process (stdlib archive/zip +
// archive/tar over xz/gzip decoders) with per-entry name sanitization and byte
// budgets, so there is no dependency on a host `tar` and no archive entry can
// write outside the destination directory or fill the disk.
package toolfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// maxToolBytes caps a single extracted binary and the compressed download itself
// (defends against a decompression bomb and is comfortably above ffmpeg's real
// size). It is a var, not a const, only so tests can shrink it to exercise the
// oversize-rejection paths without fabricating a 300 MiB fixture.
var maxToolBytes int64 = 300 << 20 // 300 MiB

// platformSpec resolves the download spec for the running platform. It is a
// package var wrapping specFor so a test can point ensure's download branch at an
// httptest server without a real network fetch. Production always uses specFor.
var platformSpec = specFor

// Tools are the resolved absolute paths to ffmpeg and ffprobe. Either is "" when
// the tool could not be located or fetched.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// ResolveConfig is the tool-resolution input, mirroring config.ToolsConfig so the
// caller need not import config here.
type ResolveConfig struct {
	FFmpegPath   string // explicit ffmpeg path/name; "" resolves automatically
	FFprobePath  string // explicit ffprobe path/name; "" resolves automatically
	AutoDownload bool   // fetch into toolsDir when not found locally
}

// Resolve locates ffmpeg and ffprobe using the full lookup order (explicit path
// -> next to the binary -> $PATH -> on-demand download into toolsDir). Any tool
// that cannot be found or fetched comes back "". It never errors: a missing tool
// is a graceful degrade the caller surfaces, not a fatal condition.
func Resolve(ctx context.Context, cfg ResolveConfig, toolsDir string, log *slog.Logger) Tools {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ffmpeg := resolveLocal("ffmpeg", cfg.FFmpegPath)
	ffprobe := resolveLocal("ffprobe", cfg.FFprobePath)
	if (ffmpeg == "" || ffprobe == "") && cfg.AutoDownload {
		dffmpeg, dffprobe := ensure(ctx, toolsDir, log)
		if ffmpeg == "" {
			ffmpeg = dffmpeg
		}
		if ffprobe == "" {
			ffprobe = dffprobe
		}
	}
	return Tools{FFmpeg: ffmpeg, FFprobe: ffprobe}
}

// resolveLocal looks for a tool without downloading: an explicit path/name (via
// LookPath, so a bare name resolves on $PATH and an absolute path is honored),
// then a copy next to the daemon binary, then $PATH by the tool's own name. It
// returns "" when none is found. An explicit-but-missing path does NOT fall back
// to $PATH - an operator who named a binary meant that one.
func resolveLocal(tool, explicit string) string {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p
		}
		return ""
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), binName(tool))
		if info, serr := os.Stat(cand); serr == nil && !info.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath(tool); err == nil {
		return p
	}
	return ""
}

// spec describes where this platform's tools come from. Either combinedURL (one
// archive holding bin/ffmpeg + bin/ffprobe, BtbN) or the separate per-tool zips
// (evermeet) are set.
type spec struct {
	combinedURL  string
	combinedKind string // "tar.xz" | "zip"
	ffmpegURL    string // separate per-tool zips (each holds the bare binary)
	ffprobeURL   string
}

const btbn = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest"

// specFor returns the download spec for an OS/arch, or ok=false when there's no
// known static source (the caller then just runs without ffmpeg).
func specFor(goos, goarch string) (spec, bool) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-linux64-lgpl.tar.xz", combinedKind: "tar.xz"}, true
	case "linux/arm64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-linuxarm64-lgpl.tar.xz", combinedKind: "tar.xz"}, true
	case "windows/amd64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-win64-lgpl.zip", combinedKind: "zip"}, true
	case "windows/arm64":
		return spec{combinedURL: btbn + "/ffmpeg-master-latest-winarm64-lgpl.zip", combinedKind: "zip"}, true
	case "darwin/amd64", "darwin/arm64":
		// evermeet ships x86_64 only; on Apple Silicon it runs under Rosetta 2,
		// which is fine to exec from an arm64 daemon.
		return spec{
			ffmpegURL:  "https://evermeet.cx/ffmpeg/getrelease/ffmpeg/zip",
			ffprobeURL: "https://evermeet.cx/ffmpeg/getrelease/ffprobe/zip",
		}, true
	}
	return spec{}, false
}

// binName is the on-disk tool filename for this OS (ffmpeg / ffmpeg.exe).
func binName(tool string) string {
	if runtime.GOOS == "windows" {
		return tool + ".exe"
	}
	return tool
}

// cached returns the cached paths for ffmpeg and ffprobe in dir, each "" if absent.
func cached(dir string) (ffmpeg, ffprobe string) {
	return cachedOne(dir, "ffmpeg"), cachedOne(dir, "ffprobe")
}

func cachedOne(dir, tool string) string {
	p := filepath.Join(dir, binName(tool))
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p
	}
	return ""
}

// ensure returns usable ffmpeg/ffprobe paths under dir, downloading a static build
// for the current platform if they aren't cached yet. Either result is "" when the
// tool couldn't be made available (offline, unsupported platform, or a failed
// self-check) - the caller degrades gracefully and retries next start.
func ensure(ctx context.Context, dir string, log *slog.Logger) (ffmpeg, ffprobe string) {
	// Verify the cached copies rather than trusting a bare stat - a download killed
	// mid-write (or a disk/FS fault) can leave a non-runnable binary that a stat
	// happily reports as present. verified discards a failing one so we fall through
	// and re-download it.
	if ffmpeg, ffprobe = cached(dir); ffmpeg != "" && ffprobe != "" {
		ffmpeg = verified(ctx, ffmpeg, log)
		ffprobe = verified(ctx, ffprobe, log)
		if ffmpeg != "" && ffprobe != "" {
			return ffmpeg, ffprobe
		}
	}
	s, ok := platformSpec(runtime.GOOS, runtime.GOARCH)
	if !ok {
		log.Warn("no ffmpeg auto-download source for this platform; install ffmpeg/ffprobe to enable the audio stages",
			"platform", runtime.GOOS+"/"+runtime.GOARCH)
		return cached(dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Warn("ffmpeg auto-download: cannot create tools dir", "dir", dir, "err", err)
		return cached(dir)
	}
	log.Info("ffmpeg/ffprobe not found locally - downloading a static build (one time)", "into", dir)
	if err := download(ctx, s, dir); err != nil {
		log.Warn("ffmpeg auto-download failed; running without it (will retry next start)", "err", err)
		return cached(dir)
	}
	ffmpeg, ffprobe = cached(dir)
	// Adopt only binaries that actually run on this machine.
	ffmpeg = verified(ctx, ffmpeg, log)
	ffprobe = verified(ctx, ffprobe, log)
	if ffmpeg != "" || ffprobe != "" {
		log.Info("ffmpeg/ffprobe downloaded", "ffmpeg", ffmpeg != "", "ffprobe", ffprobe != "")
	}
	return ffmpeg, ffprobe
}

// runSelfCheck runs `<path> <args...>` with a timeout and requires exit 0 - the
// shared sanity gate every downloaded binary must pass before it is adopted (a
// truncated, corrupt, or wrong-arch download fails here). Wrappers decide what a
// failure means: verified() removes the cached file; whisperSelfCheck leaves the
// staged file for its caller to discard.
func runSelfCheck(ctx context.Context, path string, args []string, timeout time.Duration, log *slog.Logger) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := exec.CommandContext(cctx, path, args...).Run(); err != nil { //nolint:gosec // our own downloaded tool path + fixed argv
		log.Warn("downloaded tool failed its self-check; discarding", "path", path, "err", err)
		return err
	}
	return nil
}

// requireHTTPS rejects a non-https download URL. Every artifact this package
// fetches (tool binaries, ASR models, release checksums) must ride TLS; what names
// the artifact in the error message.
func requireHTTPS(url, what string) error {
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return fmt.Errorf("%s url must be https: %q", what, url)
	}
	return nil
}

// verified returns path if `<path> -version` runs, else "" (and removes the file).
func verified(ctx context.Context, path string, log *slog.Logger) string {
	if path == "" {
		return ""
	}
	if runSelfCheck(ctx, path, []string{"-version"}, 15*time.Second, log) != nil {
		_ = os.Remove(path)
		return ""
	}
	return path
}

// download fetches and extracts ffmpeg + ffprobe into dir per the platform spec.
func download(ctx context.Context, s spec, dir string) error {
	tmp, err := os.MkdirTemp("", "audiosilo-sidecars-ffmpeg-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if s.combinedURL != "" {
		if err := fetchArchive(ctx, s.combinedURL, s.combinedKind, tmp); err != nil {
			return err
		}
	} else {
		if err := fetchArchive(ctx, s.ffmpegURL, "zip", tmp); err != nil {
			return err
		}
		if err := fetchArchive(ctx, s.ffprobeURL, "zip", tmp); err != nil {
			return err
		}
	}

	// Pull the two binaries out of whatever directory layout the archive used.
	want := map[string]bool{binName("ffmpeg"): true, binName("ffprobe"): true}
	found := 0
	err = filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !want[d.Name()] {
			return nil
		}
		if cerr := copyExec(p, filepath.Join(dir, d.Name())); cerr != nil {
			return cerr
		}
		found++
		return nil
	})
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("archive contained no ffmpeg/ffprobe binaries")
	}
	return nil
}

// fetchArchive downloads url and extracts the ffmpeg/ffprobe binaries into
// destDir. Both archive kinds are handled fully in-process (no host `tar`): zip
// via archive/zip, tar.xz via archive/tar over an xz decoder. Each path enforces
// the maxToolBytes cap and per-entry name sanitization.
func fetchArchive(ctx context.Context, url, kind, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	switch kind {
	case "tar.xz":
		// Stream the compressed archive to a bounded temp file first (fail if it
		// exceeds the cap), then decode+extract from it. Buffering to disk keeps peak
		// memory flat regardless of the archive size.
		f, err := os.CreateTemp(destDir, "dl-*.tar.xz")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		n, err := io.Copy(f, io.LimitReader(resp.Body, maxToolBytes+1))
		if err != nil {
			_ = f.Close()
			return err
		}
		if n > maxToolBytes {
			_ = f.Close()
			return fmt.Errorf("download %s exceeds %d bytes", url, maxToolBytes)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		err = extractTarXz(f, destDir)
		_ = f.Close()
		return err
	case "zip":
		return extractZip(resp.Body, destDir)
	default:
		return fmt.Errorf("unknown archive kind %q", kind)
	}
}

// extractZip writes only the ffmpeg/ffprobe binaries from a zip stream into
// destDir. It extracts by basename into destDir (never joining the archive's own
// path), so a zip-slip entry name like "../../evil" cannot escape - and rejects
// an over-cap archive up front.
func extractZip(r io.Reader, destDir string) error {
	buf, err := io.ReadAll(io.LimitReader(r, maxToolBytes+1))
	if err != nil {
		return err
	}
	if int64(len(buf)) > maxToolBytes {
		return fmt.Errorf("zip exceeds %d bytes", maxToolBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return err
	}
	want := map[string]bool{binName("ffmpeg"): true, binName("ffprobe"): true}
	for _, zf := range zr.File {
		base := filepath.Base(zf.Name)
		if zf.FileInfo().IsDir() || !want[base] {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeExec(filepath.Join(destDir, base), io.LimitReader(rc, maxToolBytes))
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// extractTarXz writes only the ffmpeg/ffprobe binaries from a tar.xz stream into
// destDir. Every entry name is sanitized (safeEntryName rejects absolute paths and
// any ".." traversal) before use, and binaries are written by basename into
// destDir, so no entry can escape. Non-tool entries are skipped; each copied
// binary is bounded by the cap.
func extractTarXz(r io.Reader, destDir string) error {
	xr, err := xz.NewReader(r)
	if err != nil {
		return fmt.Errorf("open xz: %w", err)
	}
	tr := tar.NewReader(xr)
	want := map[string]bool{binName("ffmpeg"): true, binName("ffprobe"): true}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !safeEntryName(hdr.Name) {
			return fmt.Errorf("tar entry %q is unsafe", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !want[filepath.Base(hdr.Name)] {
			continue
		}
		if err := writeExec(filepath.Join(destDir, filepath.Base(hdr.Name)), io.LimitReader(tr, maxToolBytes)); err != nil {
			return err
		}
	}
}

// safeEntryName reports whether an archive entry name is safe to extract: not
// absolute and containing no ".." element (which could traverse out of destDir).
// It is the per-entry guard the tar path enforces before touching the filesystem.
func safeEntryName(name string) bool {
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// copyExec copies src to dst as an executable.
func copyExec(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is a path inside our own temp extract dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeExec(dst, io.LimitReader(in, maxToolBytes))
}

// writeExec writes r to path as an executable (0o755). It is the common case of
// writeMode for a tool binary.
func writeExec(path string, r io.Reader) error {
	_, err := writeMode(path, r, 0o755)
	return err
}

// writeMode writes r to path with perm atomically: it streams into a sibling temp
// file and renames it into place only after a clean close, so a process killed
// mid-copy can never leave a truncated file at the final path (which a cache-hit
// check would then adopt forever, since a stat can't tell a partial file from a
// whole one). perm lets a caller preserve an archive entry's own mode (an
// executable binary vs a plain shared library beside it). It returns the byte
// count written so budget-enforcing callers can detect an over-cap entry.
func writeMode(path string, r io.Reader, perm os.FileMode) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-"+filepath.Base(path)+"-")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed into place
	n, err := io.Copy(tmp, r)                 //nolint:gosec // bounded by the caller's LimitReader
	if err != nil {
		_ = tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return n, err
	}
	return n, os.Rename(tmpName, path)
}
