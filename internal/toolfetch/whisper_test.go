package toolfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWhisperAssetFor pins the full platform+device -> asset table, including the
// linux/amd64 device variants and every platform that has NO published asset.
func TestWhisperAssetFor(t *testing.T) {
	cases := []struct {
		goos, goarch, device string
		want                 string
		ok                   bool
	}{
		{"darwin", "arm64", "metal", "whisper-cli-darwin-arm64-metal.tar.gz", true},
		{"darwin", "arm64", "cpu", "whisper-cli-darwin-arm64-metal.tar.gz", true}, // darwin ignores device
		{"linux", "amd64", "cuda", "whisper-cli-linux-amd64-cuda.tar.gz", true},
		{"linux", "amd64", "vulkan", "whisper-cli-linux-amd64-vulkan.tar.gz", true},
		{"linux", "amd64", "cpu", "whisper-cli-linux-amd64-cpu.tar.gz", true},
		{"linux", "amd64", "", "whisper-cli-linux-amd64-cpu.tar.gz", true},     // unknown device -> cpu
		{"linux", "arm64", "cuda", "whisper-cli-linux-arm64-cpu.tar.gz", true}, // arm64 has no GPU variant
		{"linux", "arm64", "cpu", "whisper-cli-linux-arm64-cpu.tar.gz", true},
		{"windows", "amd64", "cpu", "whisper-cli-windows-amd64-cpu.zip", true},
		// No published asset:
		{"darwin", "amd64", "metal", "", false},
		{"windows", "arm64", "cpu", "", false},
		{"linux", "riscv64", "cpu", "", false},
		{"plan9", "mips", "", "", false},
	}
	for _, c := range cases {
		got, ok := WhisperCLIAssetFor(c.goos, c.goarch, c.device)
		if ok != c.ok || got != c.want {
			t.Errorf("WhisperCLIAssetFor(%s/%s, %q) = %q,%v want %q,%v", c.goos, c.goarch, c.device, got, ok, c.want, c.ok)
		}
	}
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// buildTarGz builds an in-memory .tar.gz from name->body entries (regular files,
// 0755). Mirrors the tar.xz builder in toolfetch_test.go but for the gzip transport
// the whisper.cpp assets use.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// whisperTestAsset returns the asset name this platform resolves for a cpu device,
// skipping the test when there is no exec-runnable path here: Windows (the stub
// binary is a shell script) or an unsupported platform (no published asset).
func whisperTestAsset(t *testing.T) (asset, device string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("whisper-cli stub is a shell script; skip on Windows")
	}
	device = "cpu"
	asset, ok := WhisperCLIAssetFor(runtime.GOOS, runtime.GOARCH, device)
	if !ok {
		t.Skipf("no whisper-cli asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return asset, device
}

// serveWhisperRelease stands up the release server for a single asset (see
// serveWhisperMulti for several, e.g. the CPU-fallback tests).
func serveWhisperRelease(t *testing.T, asset string, archive []byte, checksums string, hits *int) {
	t.Helper()
	serveWhisperMulti(t, map[string][]byte{asset: archive}, checksums, hits)
}

// serveWhisperMulti stands up a TLS httptest server that answers the release's
// checksums.txt and every asset URL in assets, redirects whisperReleaseBase + the
// default transport at it (restored on cleanup), and counts every HTTP hit through
// *hits.
func serveWhisperMulti(t *testing.T, assets map[string][]byte, checksums string, hits *int) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			_, _ = io.WriteString(w, checksums)
			return
		}
		for name, data := range assets {
			if strings.HasSuffix(r.URL.Path, "/"+name) {
				_, _ = w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	baseRestore := whisperReleaseBase
	whisperReleaseBase = srv.URL
	t.Cleanup(func() { whisperReleaseBase = baseRestore })

	// EnsureWhisperCLI's http.Client has a nil Transport, so it uses
	// http.DefaultTransport; install the TLS server's trusting transport for the
	// test (same trick model_test.go uses).
	trRestore := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = trRestore })
}

// checksumsFor builds a sha256sum-format checksums.txt body covering every asset.
func checksumsFor(assets map[string][]byte) string {
	var sb strings.Builder
	for name, data := range assets {
		sb.WriteString(sha256hex(data) + "  " + name + "\n")
	}
	return sb.String()
}

// TestEnsureWhisperCLIHappyPath: downloads, checksum verifies, extracts EVERY file
// (binary + a bundled lib beside it), self-check passes, .meta is written, the path
// is returned - and a second call is a pure cache hit (no new HTTP). ALLOWED.
func TestEnsureWhisperCLIHappyPath(t *testing.T) {
	asset, device := whisperTestAsset(t)
	stub := "#!/bin/sh\nexit 0\n"
	archive := buildTarGz(t, map[string]string{
		whisperCLIBase:   stub,
		"libwhisper.so":  "LIBDATA",
		"ggml-metal.dir": "META", // an extra beside file, all extracted
	})
	checksums := sha256hex(archive) + "  " + asset + "\n"
	var hits int
	serveWhisperRelease(t, asset, archive, checksums, &hits)

	toolsDir := t.TempDir()
	got, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard())
	if err != nil {
		t.Fatalf("EnsureWhisperCLI: %v", err)
	}
	wantPath := filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase))
	if got != wantPath {
		t.Fatalf("path = %q, want %q", got, wantPath)
	}
	if info, err := os.Stat(got); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("binary not executable: %v", err)
	}
	// The bundled shared library landed beside the binary.
	lib, err := os.ReadFile(filepath.Join(toolsDir, whisperSubdir, "libwhisper.so"))
	if err != nil || string(lib) != "LIBDATA" {
		t.Errorf("bundled lib not extracted: err=%v body=%q", err, lib)
	}
	// The .meta sidecar records the pinned tag.
	meta, ok := readJSONSidecar[whisperMeta](metaPath(wantPath))
	if !ok || meta.Tag != WhisperCLIReleaseTag || meta.Asset != asset {
		t.Errorf("meta = %+v ok=%v, want tag %q asset %q", meta, ok, WhisperCLIReleaseTag, asset)
	}
	if hits != 2 { // checksums.txt + asset
		t.Errorf("first-download hits = %d, want 2", hits)
	}

	// Second call: pure cache hit, no new network.
	got2, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard())
	if err != nil || got2 != wantPath {
		t.Fatalf("cached EnsureWhisperCLI = %q,%v", got2, err)
	}
	if hits != 2 {
		t.Errorf("after cache hit, hits = %d, want 2 (no new requests)", hits)
	}
}

// TestEnsureWhisperCLIChecksumMismatch: a wrong digest in checksums.txt is rejected
// and nothing is adopted at the final path. DENIED.
func TestEnsureWhisperCLIChecksumMismatch(t *testing.T) {
	asset, device := whisperTestAsset(t)
	archive := buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 0\n"})
	checksums := strings.Repeat("0", 64) + "  " + asset + "\n" // wrong sum
	serveWhisperRelease(t, asset, archive, checksums, nil)

	toolsDir := t.TempDir()
	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err == nil {
		t.Fatal("expected a checksum-mismatch error")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("a mismatched download must leave nothing at the final path")
	}
}

// TestEnsureWhisperCLIMissingChecksumLine: checksums.txt without the asset's line is
// a DENY (never "skip verification").
func TestEnsureWhisperCLIMissingChecksumLine(t *testing.T) {
	asset, device := whisperTestAsset(t)
	archive := buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 0\n"})
	checksums := sha256hex(archive) + "  some-other-file.tar.gz\n" // no line for our asset
	serveWhisperRelease(t, asset, archive, checksums, nil)

	toolsDir := t.TempDir()
	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err == nil {
		t.Fatal("expected an error when the asset has no checksum line")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("nothing must be adopted without a verified checksum")
	}
}

// TestEnsureWhisperCLIUnsafeEntryRejected: a traversing tar entry is rejected even
// with a valid checksum, and nothing escapes. DENIED.
func TestEnsureWhisperCLIUnsafeEntryRejected(t *testing.T) {
	asset, device := whisperTestAsset(t)
	archive := buildTarGz(t, map[string]string{"../../../../evil": "PWNED"})
	checksums := sha256hex(archive) + "  " + asset + "\n" // checksum is valid; extraction still refuses
	serveWhisperRelease(t, asset, archive, checksums, nil)

	toolsDir := t.TempDir()
	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err == nil {
		t.Fatal("expected an unsafe-entry rejection")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(toolsDir), "evil")); err == nil {
		t.Error("a traversing entry escaped the tools dir")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("nothing must be installed from a rejected archive")
	}
}

// TestEnsureWhisperCLISelfCheckFailure: a binary that fails `--help` (exit 1) is not
// adopted. DENIED. The host platform's cpu-device asset IS the platform's CPU asset
// (darwin/arm64 metal counts: it is the only asset), so there is no fallback target
// and no retry loop - exactly one checksums.txt + one asset fetch happen.
func TestEnsureWhisperCLISelfCheckFailure(t *testing.T) {
	asset, device := whisperTestAsset(t)
	archive := buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 1\n"})
	checksums := sha256hex(archive) + "  " + asset + "\n"
	var hits int
	serveWhisperRelease(t, asset, archive, checksums, &hits)

	toolsDir := t.TempDir()
	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err == nil {
		t.Fatal("expected a self-check failure")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("a binary that fails its self-check must not be adopted")
	}
	if hits != 2 { // checksums.txt + the one asset; no CPU retry when it IS the cpu asset
		t.Errorf("hits = %d, want 2 (a cpu-asset self-check failure must not retry)", hits)
	}
}

// TestEnsureWhisperCLICPUFallback: an ACCELERATED asset that downloads and verifies
// but fails its self-check (driver too old, broken bundle) falls back to the CPU
// asset for the same platform, adopts it, and records the CPU asset in the .meta -
// which then keeps the fallback sticky (tag-gated cache hit, no re-attempt of the
// broken accelerated asset). ALLOWED. Uses the platform-injected core so the
// linux/amd64 cuda->cpu pair is exercised regardless of the test host.
func TestEnsureWhisperCLICPUFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("whisper-cli stub is a shell script; skip on Windows")
	}
	const (
		cudaAsset = "whisper-cli-linux-amd64-cuda.tar.gz"
		cpuAsset  = "whisper-cli-linux-amd64-cpu.tar.gz"
	)
	assets := map[string][]byte{
		// The accelerated build extracts fine but won't run on this machine.
		cudaAsset: buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 1\n", "libcudart.so": "LIB"}),
		cpuAsset:  buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 0\n"}),
	}
	var hits int
	serveWhisperMulti(t, assets, checksumsFor(assets), &hits)

	toolsDir := t.TempDir()
	got, err := ensureWhisperCLIPlatform(context.Background(), toolsDir, "linux", "amd64", "cuda", discard())
	if err != nil {
		t.Fatalf("ensureWhisperCLIPlatform with CPU fallback: %v", err)
	}
	wantPath := filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase))
	if got != wantPath {
		t.Fatalf("path = %q, want %q", got, wantPath)
	}
	meta, ok := readJSONSidecar[whisperMeta](metaPath(wantPath))
	if !ok || meta.Asset != cpuAsset || meta.Tag != WhisperCLIReleaseTag || !meta.Fallback {
		t.Errorf("meta = %+v ok=%v, want the adopted CPU asset %q at tag %q with Fallback=true", meta, ok, cpuAsset, WhisperCLIReleaseTag)
	}
	if hits != 4 { // (checksums + cuda) + (checksums + cpu)
		t.Errorf("fallback hits = %d, want 4", hits)
	}

	// Stickiness: the recorded Fallback keeps the next call a cache hit even though
	// the desired (cuda) asset differs from the installed one - the broken
	// accelerated asset is NOT re-attempted until the release tag bumps.
	got2, err := ensureWhisperCLIPlatform(context.Background(), toolsDir, "linux", "amd64", "cuda", discard())
	if err != nil || got2 != wantPath {
		t.Fatalf("sticky cache hit = %q,%v", got2, err)
	}
	if hits != 4 {
		t.Errorf("after the sticky cache hit, hits = %d, want 4 (no re-attempt)", hits)
	}
}

// TestEnsureWhisperCLIDeviceUpgradeRedownloads: a PLAIN install (no fallback) is
// device-sensitive. A box whose detected device was cpu installs the CPU asset as
// its desired build; when the device later becomes cuda (the user installs an
// NVIDIA driver), the cache no longer matches the desired asset and the
// accelerated build is re-downloaded - it does not stay stuck on CPU
// transcription until the next tag bump. Only a RECORDED fallback is sticky
// (covered by TestEnsureWhisperCLICPUFallback).
func TestEnsureWhisperCLIDeviceUpgradeRedownloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("whisper-cli stub is a shell script; skip on Windows")
	}
	const (
		cudaAsset = "whisper-cli-linux-amd64-cuda.tar.gz"
		cpuAsset  = "whisper-cli-linux-amd64-cpu.tar.gz"
	)
	ok := "#!/bin/sh\nexit 0\n"
	assets := map[string][]byte{
		cpuAsset:  buildTarGz(t, map[string]string{whisperCLIBase: ok}),
		cudaAsset: buildTarGz(t, map[string]string{whisperCLIBase: ok, "libcudart.so": "LIB"}),
	}
	var hits int
	serveWhisperMulti(t, assets, checksumsFor(assets), &hits)
	toolsDir := t.TempDir()

	// First install: the detected device IS cpu, so the CPU asset is the desired
	// build, not a fallback.
	if _, err := ensureWhisperCLIPlatform(context.Background(), toolsDir, "linux", "amd64", "cpu", discard()); err != nil {
		t.Fatalf("initial cpu install: %v", err)
	}
	binPath := filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase))
	if meta, mok := readJSONSidecar[whisperMeta](metaPath(binPath)); !mok || meta.Asset != cpuAsset || meta.Fallback {
		t.Fatalf("initial meta = %+v ok=%v, want plain (non-fallback) cpu asset", meta, mok)
	}
	if hits != 2 {
		t.Fatalf("initial install hits = %d, want 2", hits)
	}

	// The detected device becomes cuda: the plain CPU install no longer matches the
	// desired asset, so the accelerated build is fetched.
	got, err := ensureWhisperCLIPlatform(context.Background(), toolsDir, "linux", "amd64", "cuda", discard())
	if err != nil || got != binPath {
		t.Fatalf("device-upgrade ensure = %q,%v", got, err)
	}
	if hits != 4 {
		t.Errorf("device upgrade hits = %d, want 4 (must re-download the accelerated build)", hits)
	}
	if meta, mok := readJSONSidecar[whisperMeta](metaPath(binPath)); !mok || meta.Asset != cudaAsset || meta.Fallback {
		t.Errorf("post-upgrade meta = %+v ok=%v, want the plain cuda asset", meta, mok)
	}
}

// TestEnsureWhisperCLICPUFallbackAlsoFails: when the CPU asset ALSO fails its
// self-check, the whole ensure errors and nothing is adopted. DENIED.
func TestEnsureWhisperCLICPUFallbackAlsoFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("whisper-cli stub is a shell script; skip on Windows")
	}
	bad := map[string]string{whisperCLIBase: "#!/bin/sh\nexit 1\n"}
	assets := map[string][]byte{
		"whisper-cli-linux-amd64-cuda.tar.gz": buildTarGz(t, bad),
		"whisper-cli-linux-amd64-cpu.tar.gz":  buildTarGz(t, bad),
	}
	serveWhisperMulti(t, assets, checksumsFor(assets), nil)

	toolsDir := t.TempDir()
	if _, err := ensureWhisperCLIPlatform(context.Background(), toolsDir, "linux", "amd64", "cuda", discard()); err == nil {
		t.Fatal("expected an error when the CPU fallback also fails its self-check")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("nothing must be adopted when both assets fail their self-check")
	}
	if _, ok := readJSONSidecar[whisperMeta](metaPath(filepath.Join(toolsDir, whisperSubdir, binName(whisperCLIBase)))); ok {
		t.Error("no .meta must be written when nothing was adopted")
	}
}

// TestEnsureWhisperCLITagUpgrade: a cached binary whose .meta records an OLDER tag
// triggers a re-download (the pin changed).
func TestEnsureWhisperCLITagUpgrade(t *testing.T) {
	asset, device := whisperTestAsset(t)
	archive := buildTarGz(t, map[string]string{whisperCLIBase: "#!/bin/sh\nexit 0\n"})
	checksums := sha256hex(archive) + "  " + asset + "\n"
	var hits int
	serveWhisperRelease(t, asset, archive, checksums, &hits)

	toolsDir := t.TempDir()
	binPath := seedStaleWhisperInstall(t, toolsDir, asset)

	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err != nil {
		t.Fatalf("EnsureWhisperCLI (upgrade): %v", err)
	}
	if hits != 2 {
		t.Errorf("tag upgrade hits = %d, want 2 (should re-download)", hits)
	}
	meta, _ := readJSONSidecar[whisperMeta](metaPath(binPath))
	if meta.Tag != WhisperCLIReleaseTag {
		t.Errorf("meta tag after upgrade = %q, want %q", meta.Tag, WhisperCLIReleaseTag)
	}
}

// seedStaleWhisperInstall pre-populates toolsDir with a prior install: an
// executable whisper-cli stub plus a .meta stamped with an OLD tag (so the
// cache-hit gate misses and a refresh is attempted). Returns the binary path.
func seedStaleWhisperInstall(t *testing.T, toolsDir, asset string) string {
	t.Helper()
	dir := filepath.Join(toolsDir, whisperSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, binName(whisperCLIBase))
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	stale, _ := json.Marshal(whisperMeta{Tag: "whisper-cpp-v0.0.0-old", Asset: asset, SHA256: "deadbeef"})
	if err := os.WriteFile(metaPath(binPath), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	return binPath
}

// TestEnsureWhisperCLIOfflineKeepsStaleCache: a failed refresh (here a release
// whose checksums.txt lacks the asset, standing in for offline/broken) with a
// previously-installed binary still present degrades to that stale binary instead
// of erroring - logging the operator-visible warning - and leaves the stale .meta
// untouched, so the tag gate keeps missing and a later run retries the refresh.
// ALLOWED. (The DENIED counterpart - a failed refresh with NO cache errors - is
// TestEnsureWhisperCLIMissingChecksumLine.)
func TestEnsureWhisperCLIOfflineKeepsStaleCache(t *testing.T) {
	asset, device := whisperTestAsset(t)
	// Empty checksums.txt: every refresh fails at the verification step.
	serveWhisperRelease(t, asset, nil, "", nil)

	toolsDir := t.TempDir()
	binPath := seedStaleWhisperInstall(t, toolsDir, asset)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	got, err := EnsureWhisperCLI(context.Background(), toolsDir, device, log)
	if err != nil {
		t.Fatalf("a failed refresh with a cached binary must degrade, not error: %v", err)
	}
	if got != binPath {
		t.Errorf("path = %q, want the stale cached %q", got, binPath)
	}
	if !strings.Contains(logBuf.String(), "proceeding with the previously-installed binary") {
		t.Errorf("expected the stale-cache warning to be logged, got:\n%s", logBuf.String())
	}
	meta, ok := readJSONSidecar[whisperMeta](metaPath(binPath))
	if !ok || meta.Tag == WhisperCLIReleaseTag {
		t.Errorf("stale .meta must survive untouched (retry next run), got %+v ok=%v", meta, ok)
	}
}

// TestExtractAllTarGzBudget: extraction enforces a TOTAL uncompressed-bytes budget
// as a HARD error - an over-cap archive (one huge entry, or several small ones
// summing over the cap) is rejected outright, never silently truncated at the cap.
func TestExtractAllTarGzBudget(t *testing.T) {
	restore := maxWhisperArchiveBytes
	maxWhisperArchiveBytes = 64
	defer func() { maxWhisperArchiveBytes = restore }()

	// One entry over the whole budget.
	big := buildTarGz(t, map[string]string{whisperCLIBase: strings.Repeat("A", 100)})
	if err := extractAllTarGz(bytes.NewReader(big), t.TempDir()); err == nil {
		t.Error("an over-budget entry must be a hard error, not a truncation")
	}
	// Entries individually under the budget but summing over it.
	many := buildTarGz(t, map[string]string{
		whisperCLIBase: strings.Repeat("B", 40),
		"lib-a.so":     strings.Repeat("C", 40),
	})
	if err := extractAllTarGz(bytes.NewReader(many), t.TempDir()); err == nil {
		t.Error("entries summing over the budget must be a hard error")
	}
}

// TestEnsureWhisperCLIOversizeExtractionRejected: a checksum-valid archive whose
// UNCOMPRESSED content exceeds the cap (tiny compressed, hostile/corrupt when
// inflated - the disk-filling shape) is rejected end to end and nothing is
// adopted. DENIED.
func TestEnsureWhisperCLIOversizeExtractionRejected(t *testing.T) {
	asset, device := whisperTestAsset(t)
	restore := maxWhisperArchiveBytes
	maxWhisperArchiveBytes = 1024
	defer func() { maxWhisperArchiveBytes = restore }()

	// Highly compressible: ~4 KiB uncompressed (over the 1 KiB cap) but a tiny
	// compressed download (under it), so only the extraction budget can catch it.
	archive := buildTarGz(t, map[string]string{whisperCLIBase: strings.Repeat("A", 4096)})
	if int64(len(archive)) > maxWhisperArchiveBytes {
		t.Fatalf("fixture: compressed size %d must sit under the cap %d", len(archive), maxWhisperArchiveBytes)
	}
	serveWhisperRelease(t, asset, archive, sha256hex(archive)+"  "+asset+"\n", nil)

	toolsDir := t.TempDir()
	if _, err := EnsureWhisperCLI(context.Background(), toolsDir, device, discard()); err == nil {
		t.Fatal("an over-budget extraction must fail the ensure")
	}
	if CachedWhisperCLI(toolsDir) != "" {
		t.Error("nothing must be adopted from an over-budget archive")
	}
}

// TestExtractAllTarGzModesAndTraversal: extract-all preserves the executable bit
// per entry (an exec entry -> 0755, a plain entry -> 0644) and rejects traversal.
func TestExtractAllTarGzModesAndTraversal(t *testing.T) {
	// Mixed-mode archive built by hand so one entry is non-executable.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name string
		mode int64
		body string
	}{
		{whisperCLIBase, 0o755, "BIN"},
		{"data.txt", 0o644, "DATA"},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractAllTarGz(bytes.NewReader(buf.Bytes()), dir); err != nil {
		t.Fatalf("extractAllTarGz: %v", err)
	}
	binInfo, _ := os.Stat(filepath.Join(dir, whisperCLIBase))
	if binInfo == nil || binInfo.Mode().Perm()&0o100 == 0 {
		t.Error("executable entry should keep its exec bit")
	}
	dataInfo, _ := os.Stat(filepath.Join(dir, "data.txt"))
	if dataInfo == nil || dataInfo.Mode().Perm()&0o111 != 0 {
		t.Errorf("plain entry should not be executable, got %v", dataInfo.Mode().Perm())
	}

	// A traversing entry is rejected outright.
	evil := buildTarGz(t, map[string]string{"../../../../etc/evil": "PWNED"})
	if err := extractAllTarGz(bytes.NewReader(evil), t.TempDir()); err == nil {
		t.Error("extractAllTarGz accepted a traversing entry")
	}
}

// TestParseChecksums exercises the sha256sum-format parser: two-space form, a
// binary-mode leading '*', a path prefix, and a missing asset.
func TestParseChecksums(t *testing.T) {
	text := "abc123  whisper-cli-linux-amd64-cpu.tar.gz\n" +
		"def456 *whisper-cli-darwin-arm64-metal.tar.gz\n" +
		"# a comment\n" +
		"789aaa  ./nested/whisper-cli-windows-amd64-cpu.zip\n"
	for _, c := range []struct {
		asset, want string
		ok          bool
	}{
		{"whisper-cli-linux-amd64-cpu.tar.gz", "abc123", true},
		{"whisper-cli-darwin-arm64-metal.tar.gz", "def456", true},
		{"whisper-cli-windows-amd64-cpu.zip", "789aaa", true},
		{"whisper-cli-linux-arm64-cpu.tar.gz", "", false},
	} {
		got, ok := parseChecksums(text, c.asset)
		if ok != c.ok || got != c.want {
			t.Errorf("parseChecksums(%q) = %q,%v want %q,%v", c.asset, got, ok, c.want, c.ok)
		}
	}
}
