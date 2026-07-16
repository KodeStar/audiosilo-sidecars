// Package toolfetch resolves and, when necessary, fetches the external media
// tools the audio stages need (ffmpeg for the chapter FLAC split, ffprobe for
// inspect). It is ported from audiosilo-server's internal/toolfetch and trimmed
// to the two tools M2 uses.
//
// Resolve owns the full lookup order: an explicit configured path, then a copy
// next to the daemon binary, then $PATH, and only if none of those turn up a tool
// (and auto-download is enabled) does it fall back to ensure, which caches a
// static build under <data>/tools/ and reuses it forever.
//
// Everything degrades gracefully: offline, auto-download disabled, or an
// unsupported platform just means the tool is absent (an audio stage then fails
// that book while the rest of the daemon keeps working) and a retry on the next
// start. Integrity: downloads are HTTPS-only from pinned, reputable hosts (GitHub
// release assets from BtbN's FFmpeg-Builds for Linux/Windows; evermeet.cx for
// macOS), and every downloaded binary is sanity-checked by running `-version`
// before it is adopted (there is no digest pinning).
package toolfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// maxToolBytes caps a single extracted binary (defends against a decompression
// bomb and is comfortably above ffmpeg's real size).
const maxToolBytes = 300 << 20 // 300 MiB

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
	s, ok := specFor(runtime.GOOS, runtime.GOARCH)
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

// verified returns path if `<path> -version` runs, else "" (and removes the file).
func verified(ctx context.Context, path string, log *slog.Logger) string {
	if path == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(cctx, path, "-version").Run(); err != nil {
		log.Warn("downloaded tool failed its self-check; discarding", "path", path, "err", err)
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

// fetchArchive downloads url and extracts it into destDir. tar.xz is handled by
// shelling out to the system tar (present on Linux/macOS); zip uses the stdlib.
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
		f, err := os.CreateTemp(destDir, "dl-*.tar.xz")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		// -J = xz; tar contains the archive's path safety.
		//nolint:gosec // fixed argv (tar + our own temp paths), no shell, no user input
		cmd := exec.CommandContext(ctx, "tar", "-xJf", f.Name(), "-C", destDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tar extract: %v: %s", err, out)
		}
		return nil
	case "zip":
		return extractZip(resp.Body, destDir)
	default:
		return fmt.Errorf("unknown archive kind %q", kind)
	}
}

// extractZip writes only the ffmpeg/ffprobe binaries from a zip stream into
// destDir (basename only - avoids zip-slip and skips the rest of the archive).
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

// copyExec copies src to dst as an executable.
func copyExec(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is a path inside our own temp extract dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeExec(dst, io.LimitReader(in, maxToolBytes))
}

// writeExec writes r to path (0o755) atomically: it streams into a sibling temp
// file and renames it into place only after a clean close, so a process killed
// mid-copy can never leave a truncated binary at the final path (which Ensure
// would then adopt forever, since a stat can't tell a partial file from a whole
// one).
func writeExec(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()  // no-op once renamed into place
	if _, err := io.Copy(tmp, r); err != nil { //nolint:gosec // bounded by the caller's LimitReader
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // tool must be executable
		return err
	}
	return os.Rename(tmpName, path)
}
