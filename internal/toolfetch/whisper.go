package toolfetch

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// WhisperCLIReleaseTag pins the GitHub release of THIS repo whose assets hold the
// prebuilt whisper.cpp `whisper-cli` binaries (a sibling CI job publishes them).
// Bumping the tag is a deliberate upgrade: EnsureWhisperCLI treats a cached binary
// stamped with a different tag as stale and re-downloads. The build variant is
// baked into the pinned build, hence the trailing "-1" revision.
const WhisperCLIReleaseTag = "whisper-cpp-v1.9.1-1"

// Device names the informational accelerator a whisper-cli build targets. The
// vocabulary lives here because the release asset table (WhisperCLIAssetFor) is
// keyed on it; the asr package aliases these constants for its capability
// reporting.
const (
	DeviceMetal  = "metal"
	DeviceCUDA   = "cuda"
	DeviceVulkan = "vulkan"
	DeviceCPU    = "cpu"
)

// whisperSubdir is the directory under <data>/tools that holds the extracted
// whisper.cpp distribution (the whisper-cli binary plus any shared libraries the
// build variant bundles beside it, e.g. the CUDA runtime).
const whisperSubdir = "whisper-cpp"

// whisperCLIBase is the whisper.cpp CLI binary's base name (binName adds .exe on
// Windows).
const whisperCLIBase = "whisper-cli"

// maxWhisperArchiveBytes caps both the compressed archive download and any single
// extracted file (defends against a decompression bomb / a runaway error body).
// The heaviest variant is the CUDA tarball (a few hundred MiB compressed, bundling
// libcudart/libcublas), so 2 GiB is a comfortable ceiling. It is a var, not a
// const, only so tests can shrink it to exercise the oversize-rejection paths.
var maxWhisperArchiveBytes int64 = 2 << 30 // 2 GiB

// maxChecksumsBytes caps the checksums.txt fetch: it is a short text manifest, so a
// far smaller ceiling catches a wrong/HTML body without reading a huge stream.
const maxChecksumsBytes = 1 << 20 // 1 MiB

// whisperReleaseBase is the GitHub Releases download root for this repo's assets.
// It is a package var (like platformSpec) so a test can point the download branch
// at an httptest server. Production always uses the real GitHub host. The full URLs
// are <base>/<tag>/<asset> and <base>/<tag>/checksums.txt.
var whisperReleaseBase = "https://github.com/KodeStar/audiosilo-sidecars/releases/download"

// whisperMeta is the sidecar written LAST, after the binary is fully in place, so a
// process killed mid-install never leaves a final dir the cache-hit path would
// trust. It records the pinned tag (an upgrade invalidates the cache), the asset it
// came from, and the verified sha256, for diagnostics/auditing.
type whisperMeta struct {
	Tag    string `json:"tag"`
	Asset  string `json:"asset"`
	SHA256 string `json:"sha256"`
}

// WhisperCLIAssetFor maps a platform+device to the pinned release asset that
// carries a matching whisper-cli build, or ok=false when this repo publishes no
// asset for it (darwin/amd64, windows/arm64, exotic OS/arch). A false result is a
// graceful "unavailable", never an error. device is the informational accelerator
// the ASR backend detected (the Device* constants); it only differentiates the
// linux/amd64 variants, which ship separate CUDA/Vulkan/CPU builds. The ASR
// backend also uses this pure table check to advertise "will be downloaded on
// first use" before any network I/O.
func WhisperCLIAssetFor(goos, goarch, device string) (name string, ok bool) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "whisper-cli-darwin-arm64-metal.tar.gz", true
	case "linux/amd64":
		switch device {
		case DeviceCUDA:
			return "whisper-cli-linux-amd64-cuda.tar.gz", true
		case DeviceVulkan:
			return "whisper-cli-linux-amd64-vulkan.tar.gz", true
		default:
			return "whisper-cli-linux-amd64-cpu.tar.gz", true
		}
	case "linux/arm64":
		// No GPU variants published for arm64: CPU regardless of the detected device.
		return "whisper-cli-linux-arm64-cpu.tar.gz", true
	case "windows/amd64":
		return "whisper-cli-windows-amd64-cpu.zip", true
	default:
		return "", false
	}
}

// CachedWhisperCLI returns the path to an already-installed whisper-cli under
// toolsDir, or "" if absent. It is the resolution step the ASR backend tries after
// the local lookup (explicit -> beside the binary -> $PATH): a cache populated by a
// prior EnsureWhisperCLI, reused with no network I/O. It deliberately does NOT
// consult the .meta - it is a bare "is a binary there" probe, so callers that
// manage the cache must run EnsureWhisperCLI first (its .meta-gated cache-hit path
// is what enforces a tag upgrade and repairs a meta-less partial install) and fall
// back to this only as the offline/stale answer.
func CachedWhisperCLI(toolsDir string) string {
	p := filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase))
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p
	}
	return ""
}

// EnsureWhisperCLI returns a usable local path to the whisper.cpp `whisper-cli`
// binary, downloading and installing it from the pinned GitHub release when the
// cache is absent or stale. The extracted distribution lives under
// <toolsDir>/whisper-cpp/ (the binary plus any shared libraries the variant bundles
// beside it, resolved by an $ORIGIN RPATH). device selects the linux/amd64 build
// variant (cuda/vulkan/cpu).
//
// Cache hit: a .meta sidecar stamped with the pinned tag AND the binary present
// returns immediately with NO network I/O. A tag mismatch (an upgrade) or a missing
// binary re-downloads.
//
// Integrity: HTTPS-only; the asset's sha256 is looked up in the release's
// checksums.txt (a missing line is a hard DENY, never "skip verification") and
// verified byte-for-byte while streaming - a mismatch adopts nothing. Extraction
// runs into a TEMP dir with per-entry name sanitization and size caps; the binary
// is self-checked (`--help` must exit 0) before the temp dir is atomically moved
// into place; the .meta is written LAST.
//
// Degradation: a failed refresh (offline, a broken release) with a previously-
// installed binary still present returns that stale binary with a warning - the
// stale .meta keeps failing the tag gate, so the refresh is retried on a later
// run. Only when nothing is usable does it return "", err (the caller degrades
// gracefully and retries next run).
//
// CPU fallback: when an ACCELERATED asset downloads and verifies fine but fails the
// self-check on this machine (an NVIDIA driver too old for the CUDA build, a broken
// shared-library bundle - the realistic escape, since CI runners have no GPU to
// execute the CUDA leg), the platform's CPU asset is fetched and adopted instead of
// leaving the user with no binary at all. The .meta records the ADOPTED asset, and
// because the cache-hit gate compares only the tag, the fallback stays STICKY until
// the next release-tag bump - so a broken accelerated build is not re-attempted on
// every book, and a user who fixes their driver picks the accelerated build back up
// at the next tag.
func EnsureWhisperCLI(ctx context.Context, toolsDir, device string, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return ensureWhisperCLIPlatform(ctx, toolsDir, runtime.GOOS, runtime.GOARCH, device, log)
}

// ensureWhisperCLIPlatform is EnsureWhisperCLI's core with the platform injected,
// so tests can exercise the asset selection + CPU fallback for platforms other than
// the test host (production always passes runtime.GOOS/GOARCH).
func ensureWhisperCLIPlatform(ctx context.Context, toolsDir, goos, goarch, device string, log *slog.Logger) (string, error) {
	binPath := filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase))

	// Cache hit: the sidecar's tag matches the pin and the binary is present. The
	// .meta is written last, so its presence proves a complete prior install.
	if meta, ok := readJSONSidecar[whisperMeta](metaPath(binPath)); ok && meta.Tag == WhisperCLIReleaseTag {
		if info, err := os.Stat(binPath); err == nil && !info.IsDir() {
			return binPath, nil
		}
	}

	if err := refreshWhisperCLI(ctx, toolsDir, goos, goarch, device, log); err != nil {
		// Offline/stale degrade (matching ffmpeg's ensure): a failed refresh with a
		// previously-installed binary still present proceeds on the stale binary
		// rather than leaving the caller with nothing. The untouched stale .meta
		// keeps failing the tag gate, so a later run retries the refresh.
		if cached := CachedWhisperCLI(toolsDir); cached != "" {
			log.Warn("whisper-cli refresh failed; proceeding with the previously-installed binary",
				"path", cached, "err", err)
			return cached, nil
		}
		return "", err
	}
	return binPath, nil
}

// refreshWhisperCLI downloads and installs the pinned release's asset for the
// platform - retrying once with the CPU asset when an accelerated build fails its
// self-check - and writes the .meta LAST, so only a complete install is ever
// trusted by the cache-hit gate.
func refreshWhisperCLI(ctx context.Context, toolsDir, goos, goarch, device string, log *slog.Logger) error {
	asset, ok := WhisperCLIAssetFor(goos, goarch, device)
	if !ok {
		return fmt.Errorf("no whisper-cli release asset for %s/%s", goos, goarch)
	}
	finalDir := filepath.Join(toolsDir, whisperSubdir)
	binPath := filepath.Join(finalDir, binName(whisperCLIBase))

	if err := os.MkdirAll(toolsDir, 0o750); err != nil {
		return err
	}

	sum, err := installWhisperAsset(ctx, toolsDir, finalDir, asset, log)
	if err != nil {
		// Only a SELF-CHECK failure (binary verified + extracted but won't run here)
		// falls back to the CPU build; checksum/network/extract failures do not - they
		// would fail identically for any asset and must surface as-is.
		cpuAsset, cpuOK := WhisperCLIAssetFor(goos, goarch, DeviceCPU)
		if !errors.Is(err, errWhisperSelfCheck) || !cpuOK || cpuAsset == asset {
			return err
		}
		log.Warn("whisper-cli accelerated build failed its self-check on this machine; falling back to the CPU build",
			"asset", asset, "cpu_asset", cpuAsset, "err", err)
		asset = cpuAsset
		if sum, err = installWhisperAsset(ctx, toolsDir, finalDir, asset, log); err != nil {
			return err
		}
	}

	// Write the sidecar LAST: only now is the final dir a complete, trusted install.
	// It records the asset actually adopted (the CPU one after a fallback).
	if err := writeJSONSidecar(metaPath(binPath), whisperMeta{Tag: WhisperCLIReleaseTag, Asset: asset, SHA256: sum}); err != nil {
		return err
	}
	log.Info("whisper-cli ready", "path", binPath, "asset", asset, "tag", WhisperCLIReleaseTag)
	return nil
}

// installWhisperAsset runs one asset through the full download -> checksum-verify ->
// extract -> self-check -> atomic-install sequence into finalDir and returns the
// verified sha256. It does NOT write the .meta (the caller does, last). A self-check
// failure is distinguishable via errors.Is(err, errWhisperSelfCheck) so the caller
// can decide to fall back to the CPU asset; nothing is installed on any failure.
func installWhisperAsset(ctx context.Context, toolsDir, finalDir, asset string, log *slog.Logger) (string, error) {
	// Download the asset into a bounded temp file, verifying its sha256 against the
	// release's checksums.txt as it streams. Nothing is trusted before this passes.
	archive, err := os.CreateTemp(toolsDir, ".whisper-dl-*")
	if err != nil {
		return "", err
	}
	archivePath := archive.Name()
	_ = archive.Close()
	defer func() { _ = os.Remove(archivePath) }()

	sum, err := downloadWhisperAsset(ctx, WhisperCLIReleaseTag, asset, archivePath, log)
	if err != nil {
		return "", err
	}

	// Extract into a temp dir (a sibling of finalDir under toolsDir, so the later
	// move is a same-filesystem rename), self-check the binary, then swap it in.
	extractDir, err := os.MkdirTemp(toolsDir, ".whisper-ex-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(extractDir) }() // no-op once renamed into place

	if err := extractWhisperArchive(asset, archivePath, extractDir); err != nil {
		return "", err
	}
	stagedBin := filepath.Join(extractDir, binName(whisperCLIBase))
	if info, serr := os.Stat(stagedBin); serr != nil || info.IsDir() {
		return "", fmt.Errorf("whisper-cli asset %q did not contain %s", asset, binName(whisperCLIBase))
	}
	// Always mark the binary executable (an archive entry may drop the bit), then
	// self-check it before adopting anything.
	if err := os.Chmod(stagedBin, 0o755); err != nil { //nolint:gosec // the whisper-cli binary must be executable
		return "", err
	}
	if err := whisperSelfCheck(ctx, stagedBin, log); err != nil {
		return "", err
	}

	if err := installWhisperDir(extractDir, finalDir); err != nil {
		return "", err
	}
	return sum, nil
}

// whisperAssetURL and whisperChecksumsURL build the two release URLs from the base.
func whisperAssetURL(tag, asset string) string {
	return whisperReleaseBase + "/" + tag + "/" + asset
}
func whisperChecksumsURL(tag string) string {
	return whisperReleaseBase + "/" + tag + "/checksums.txt"
}

// downloadWhisperAsset fetches checksums.txt, resolves the asset's expected sha256
// (a missing line is a hard error - never a silent skip), then streams the asset
// into destPath while computing its sha256, and fails unless the two match. It
// returns the verified hex digest. Both URLs must be https.
func downloadWhisperAsset(ctx context.Context, tag, asset, destPath string, log *slog.Logger) (string, error) {
	want, err := fetchWhisperChecksum(ctx, tag, asset)
	if err != nil {
		return "", err
	}

	url := whisperAssetURL(tag, asset)
	if err := requireHTTPS(url, "whisper-cli"); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(destPath) //nolint:gosec // destPath is our own temp file under toolsDir
	if err != nil {
		return "", err
	}
	h := sha256.New()
	pw := &progressWriter{log: log, nextLog: progressStep}
	// Bound the copy at cap+1 so an over-cap body trips the ceiling rather than
	// filling the disk, and hash every byte we accept.
	n, err := io.Copy(io.MultiWriter(f, h, pw), io.LimitReader(resp.Body, maxWhisperArchiveBytes+1))
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxWhisperArchiveBytes {
		return "", fmt.Errorf("whisper-cli asset %s exceeds %d bytes", asset, maxWhisperArchiveBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("whisper-cli asset %s checksum mismatch: got %s want %s", asset, got, want)
	}
	log.Info("whisper-cli asset verified", "asset", asset, "bytes", n, "sha256", got)
	return got, nil
}

// fetchWhisperChecksum downloads checksums.txt and returns the hex sha256 recorded
// for asset. It errors if the manifest is unreachable OR lacks a line for the asset
// (a missing checksum is a DENY: we never adopt an unverifiable download).
func fetchWhisperChecksum(ctx context.Context, tag, asset string) (string, error) {
	url := whisperChecksumsURL(tag)
	if err := requireHTTPS(url, "whisper-cli checksums"); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBytes))
	if err != nil {
		return "", err
	}
	if sum, ok := parseChecksums(string(body), asset); ok {
		return sum, nil
	}
	return "", fmt.Errorf("checksums.txt has no entry for %q (refusing to adopt an unverifiable asset)", asset)
}

// parseChecksums finds asset's hex digest in sha256sum-format text: each line is
// "<hex>  <filename>" (the filename may carry a leading "*" for binary mode, and a
// path prefix). It matches on the filename's basename so a "./" or directory prefix
// still resolves. ok=false when no line names the asset.
func parseChecksums(text, asset string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := fields[0]
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if filepath.Base(filepath.ToSlash(name)) == asset {
			return sum, true
		}
	}
	return "", false
}

// extractWhisperArchive extracts EVERY regular file from the asset archive into
// destDir (the FLAT distribution layout: whisper-cli plus any bundled shared
// libraries beside it), preserving each entry's executable bit. The kind is chosen
// by the asset's extension: .zip for Windows, .tar.gz otherwise.
func extractWhisperArchive(asset, archivePath, destDir string) error {
	f, err := os.Open(archivePath) //nolint:gosec // archivePath is our own temp file under toolsDir
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if strings.HasSuffix(strings.ToLower(asset), ".zip") {
		// The archive is already on disk (and its download was capped), so read the
		// central directory straight off the file instead of buffering it in memory.
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() > maxWhisperArchiveBytes {
			return fmt.Errorf("zip exceeds %d bytes", maxWhisperArchiveBytes)
		}
		return extractAllZip(f, info.Size(), destDir)
	}
	return extractAllTarGz(f, destDir)
}

// extractAllTarGz extracts every regular file from a gzip-compressed tar into
// destDir by basename (never joining the archive's own path), rejecting any unsafe
// entry name outright and capping each file. An entry that carries an executable
// mode bit is written 0o755, else 0o644 - so a bundled shared library is not
// needlessly executable while the binary is.
func extractAllTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	found := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
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
		perm := archiveEntryPerm(hdr.Mode&0o111 != 0)
		if err := writeMode(filepath.Join(destDir, filepath.Base(hdr.Name)), io.LimitReader(tr, maxWhisperArchiveBytes), perm); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("archive contained no files")
	}
	return nil
}

// extractAllZip extracts every regular file from an on-disk zip into destDir by
// basename, with the same safety (basename-only, per-file cap, executable bit
// preserved) as the tar path. The caller enforces the archive size cap.
func extractAllZip(ra io.ReaderAt, size int64, destDir string) error {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return err
	}
	found := 0
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if !safeEntryName(zf.Name) {
			return fmt.Errorf("zip entry %q is unsafe", zf.Name)
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		perm := archiveEntryPerm(zf.FileInfo().Mode()&0o111 != 0)
		err = writeMode(filepath.Join(destDir, filepath.Base(zf.Name)), io.LimitReader(rc, maxWhisperArchiveBytes), perm)
		_ = rc.Close()
		if err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("archive contained no files")
	}
	return nil
}

// archiveEntryPerm maps an archive entry's executable bit to a full file mode.
func archiveEntryPerm(executable bool) os.FileMode {
	if executable {
		return 0o755
	}
	return 0o644
}

// errWhisperSelfCheck marks a self-check failure: the binary downloaded, verified,
// and extracted fine but would not run on THIS machine (`--help` non-zero). It is
// the one failure class that justifies falling back to the CPU asset - every other
// failure (checksum, network, extract) would repeat identically for any asset.
var errWhisperSelfCheck = errors.New("whisper-cli self-check (--help) failed")

// whisperSelfCheck runs the shared self-check with `--help` before a staged binary
// is adopted. Unlike verified(), it does NOT remove the failing binary - it lives
// in a staged temp dir the caller discards - and it wraps errWhisperSelfCheck so
// the caller can distinguish "won't run here" (CPU-fallback eligible) from a
// download/extract problem.
func whisperSelfCheck(ctx context.Context, path string, log *slog.Logger) error {
	if err := runSelfCheck(ctx, path, []string{"--help"}, 30*time.Second, log); err != nil {
		return fmt.Errorf("%w: %v", errWhisperSelfCheck, err)
	}
	return nil
}

// installWhisperDir atomically replaces finalDir with extractDir: it removes any
// prior install, then renames the staged dir into place. The two are same-parent
// siblings under toolsDir by construction, so the rename cannot cross filesystems -
// a failure is a real error to surface, not one to paper over.
func installWhisperDir(extractDir, finalDir string) error {
	if err := os.RemoveAll(finalDir); err != nil {
		return err
	}
	if err := os.Rename(extractDir, finalDir); err != nil {
		return fmt.Errorf("install whisper-cli dir: %w", err)
	}
	return nil
}
