package asr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/toolfetch"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeScript writes an executable shell script and returns its path.
func writeScript(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	return path
}

// fakePython is a single fake python interpreter used for the whole mlx chain. It
// handles the exact invocations the backend now makes THROUGH the python binary
// (never a console script - see mlxwhisper.go): `--version`; `-m venv DIR` (copies
// itself to DIR/bin/python so the venv's python is this same fake); `-m pip install
// ...` (creates a stub mlx_whisper console script beside itself so EnsureReady's
// presence check passes); and `-m mlx_whisper.cli ...` (emits an openai-format raw
// JSON with a NaN, exercising the downstream sanitize, at --output-dir). The venv
// lives under a data dir whose path may contain a space, so this must never rely on
// an unquoted shebang.
const fakePython = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "Python 3.99.0 (fake)"; exit 0; fi
if [ "$1" = "-m" ]; then
  MOD="$2"; shift 2
  case "$MOD" in
    venv)
      DIR="$1"
      mkdir -p "$DIR/bin"
      cp "$0" "$DIR/bin/python"
      chmod +x "$DIR/bin/python"
      exit 0
      ;;
    pip)
      SELFDIR=$(dirname "$0")
      printf '#!/bin/sh\nexit 0\n' > "$SELFDIR/mlx_whisper"
      chmod +x "$SELFDIR/mlx_whisper"
      exit 0
      ;;
    mlx_whisper.cli)
      OUTDIR=""
      AUDIO=""
      while [ $# -gt 0 ]; do
        case "$1" in
          --output-dir) OUTDIR="$2"; shift 2;;
          --model|--language|--output-format|--word-timestamps|--verbose|--initial-prompt) shift 2;;
          -*) shift;;
          *) AUDIO="$1"; shift;;
        esac
      done
      STEM=$(basename "$AUDIO"); STEM=${STEM%.*}
      mkdir -p "$OUTDIR"
      printf '{"text":" fake","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" fake","avg_logprob":NaN,"words":[]}]}\n' > "$OUTDIR/$STEM.json"
      exit 0
      ;;
  esac
fi
exit 1
`

// fakeMLXChain puts the fakePython interpreter on PATH as python3, so EnsureReady
// builds a venv (whose bin/python is this same fake) and Transcribe drives it via
// `python -m mlx_whisper.cli`.
func fakeMLXChain(t *testing.T) {
	t.Helper()
	pathDir := filepath.Join(t.TempDir(), "bin")
	writeScript(t, filepath.Join(pathDir, "python3"), fakePython)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMLXDetect(t *testing.T) {
	cap, err := newMLXWhisper(SelectConfig{}).Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if cap.Backend != IDMLXWhisper {
		t.Errorf("backend = %q", cap.Backend)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// python3 is present in this repo's dev/CI env; if not, availability is false
		// with a clear detail - either way the device is metal.
		if cap.Device != DeviceMetal {
			t.Errorf("device = %q, want metal", cap.Device)
		}
		if !cap.Available && cap.Detail == "" {
			t.Error("unavailable mlx must carry a Detail")
		}
	} else if cap.Available {
		t.Error("mlx-whisper must be unavailable off darwin/arm64")
	}
}

func TestMLXEnsureReadyAndTranscribe(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("mlx-whisper backend is gated to darwin/arm64")
	}
	fakeMLXChain(t)
	dataDir := t.TempDir()
	b := newMLXWhisper(SelectConfig{DataDir: dataDir, Log: discardLogger()})

	cap, _ := b.Detect(context.Background())
	if !cap.Available {
		t.Fatalf("fake python3 should make mlx available: %+v", cap)
	}
	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	// Idempotent: a second call is a no-op (venv already provisioned).
	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady (2nd): %v", err)
	}
	// Transcribe writes chNNN.json into OutDir named from the FLAC stem.
	outDir := filepath.Join(dataDir, "raw")
	audio := filepath.Join(dataDir, "ch007.flac")
	if err := os.WriteFile(audio, []byte("flac"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := b.Transcribe(context.Background(), Job{Audio: audio, OutDir: outDir, Chapter: 7, Language: "en"}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "ch007.json")); err != nil {
		t.Errorf("raw output ch007.json missing: %v", err)
	}
}

func TestMLXEnsureReadyRequiresPython(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("mlx-whisper backend is gated to darwin/arm64")
	}
	// A PATH with no python3 makes EnsureReady fail clearly.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	b := newMLXWhisper(SelectConfig{DataDir: t.TempDir(), Log: discardLogger()})
	if err := b.EnsureReady(context.Background(), b.dataDir); err == nil {
		t.Fatal("EnsureReady should fail without python3")
	}
}

// TestMLXVenvInvocationWithSpaceInPath proves the whole venv flow survives a <data>
// dir whose path contains a space - the reason the backend invokes python directly
// instead of the console scripts (an unquoted console-script shebang breaks on a
// spaced interpreter path). It runs EnsureReady then Transcribe under such a path
// and asserts the raw output lands. darwin/arm64-gated like the other mlx tests
// (EnsureReady guards on the platform); the exec chain itself is the fakePython.
func TestMLXVenvInvocationWithSpaceInPath(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("mlx-whisper backend is gated to darwin/arm64")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes are unix-only")
	}
	fakeMLXChain(t)
	dataDir := filepath.Join(t.TempDir(), "data dir with space")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	b := newMLXWhisper(SelectConfig{DataDir: dataDir, Log: discardLogger()})
	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady under spaced path: %v", err)
	}
	outDir := filepath.Join(dataDir, "raw")
	audio := filepath.Join(dataDir, "ch003.flac")
	if err := os.WriteFile(audio, []byte("flac"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := b.Transcribe(context.Background(), Job{Audio: audio, OutDir: outDir, Chapter: 3, Language: "en"}); err != nil {
		t.Fatalf("Transcribe under spaced path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "ch003.json")); err != nil {
		t.Errorf("raw output ch003.json missing under spaced path: %v", err)
	}
}

// fakeWhisperCLI writes a fake whisper-cli that emits a whisper.cpp -ojf raw JSON
// at the -of prefix, and returns its path.
func fakeWhisperCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writeScript(t, filepath.Join(dir, "whisper-cli"), `#!/bin/sh
OF=""
while [ $# -gt 0 ]; do
  case "$1" in
    -of) OF="$2"; shift 2;;
    -m|-f|-l|--prompt) shift 2;;
    -oj|-ojf) shift;;
    *) shift;;
  esac
done
printf '{"result":{"language":"en"},"transcription":[{"offsets":{"from":0,"to":1000},"text":" fake","tokens":[{"text":" fake","offsets":{"from":0,"to":500},"p":0.9}]}]}\n' > "$OF.json"
`)
}

// TestMLXEnsurePinnedVersion exercises the in-place version-pin enforcement on an
// existing venv (no platform guard, so it runs on any unix): a matching marker is
// a no-op, while a missing or mismatched marker triggers a pip reinstall of the
// pinned version and rewrites the marker - without rebuilding the venv.
func TestMLXEnsurePinnedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake pip is unix-only")
	}
	newFakeVenv := func(t *testing.T) (*mlxWhisper, string, string) {
		t.Helper()
		dataDir := t.TempDir()
		b := newMLXWhisper(SelectConfig{DataDir: dataDir, Log: discardLogger()})
		invocations := filepath.Join(dataDir, "pip-invocations")
		// A fake venv python that records a `-m pip install ...` invocation (and its
		// args) into a sentinel file - reinstall now runs `python -m pip`, not the
		// pip console script.
		writeScript(t, b.venvBin(dataDir, "python"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-m" ] && [ "$2" = "pip" ]; then
  shift 2
  echo "$@" >> %q
  exit 0
fi
exit 1
`, invocations))
		return b, dataDir, invocations
	}
	ran := func(sentinel string) bool { _, err := os.Stat(sentinel); return err == nil }

	t.Run("matching marker is a no-op", func(t *testing.T) {
		b, dataDir, sentinel := newFakeVenv(t)
		if err := b.writeVersionMarker(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := b.ensurePinnedVersion(context.Background(), dataDir); err != nil {
			t.Fatalf("ensurePinnedVersion: %v", err)
		}
		if ran(sentinel) {
			t.Error("pip should NOT run when the marker already matches the pin")
		}
	})

	t.Run("missing marker triggers reinstall", func(t *testing.T) {
		b, dataDir, sentinel := newFakeVenv(t)
		if err := b.ensurePinnedVersion(context.Background(), dataDir); err != nil {
			t.Fatalf("ensurePinnedVersion: %v", err)
		}
		if !ran(sentinel) {
			t.Error("pip should run to install the pin when no marker exists")
		}
		if got := b.readVersionMarker(dataDir); got != mlxWhisperVersion {
			t.Errorf("marker after reinstall = %q, want %q", got, mlxWhisperVersion)
		}
	})

	t.Run("mismatched marker triggers reinstall", func(t *testing.T) {
		b, dataDir, sentinel := newFakeVenv(t)
		if err := fsutil.WriteFileAtomic(b.versionMarkerPath(dataDir), []byte("0.0.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.ensurePinnedVersion(context.Background(), dataDir); err != nil {
			t.Fatalf("ensurePinnedVersion: %v", err)
		}
		if !ran(sentinel) {
			t.Error("pip should run to install the pin when the marker is stale")
		}
		if got := b.readVersionMarker(dataDir); got != mlxWhisperVersion {
			t.Errorf("marker after reinstall = %q, want %q", got, mlxWhisperVersion)
		}
	})
}

func TestWhisperCppDetectAndTranscribe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fakes are unix-only")
	}
	cli := fakeWhisperCLI(t)
	dataDir := t.TempDir()
	b := newWhisperCpp(SelectConfig{WhisperCLIPath: cli, DataDir: dataDir, Log: discardLogger()})

	cap, _ := b.Detect(context.Background())
	if !cap.Available {
		t.Fatalf("fake whisper-cli should make the backend available: %+v", cap)
	}
	if cap.Backend != IDWhisperCpp {
		t.Errorf("backend = %q", cap.Backend)
	}

	// Pre-seed the model file (EnsureReady would download the real ~1.6 GiB one).
	model := b.modelPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(model), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("ggml"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}

	outDir := filepath.Join(dataDir, "raw")
	audio := filepath.Join(dataDir, "ch002.flac")
	if err := os.WriteFile(audio, []byte("flac"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := b.Transcribe(context.Background(), Job{Audio: audio, OutDir: outDir, Chapter: 2, Language: "en"}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "ch002.json")); err != nil {
		t.Errorf("raw output ch002.json missing: %v", err)
	}
}

func TestWhisperCppUnavailableWithoutCLI(t *testing.T) {
	b := newWhisperCpp(SelectConfig{WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: t.TempDir(), Log: discardLogger()})
	cap, _ := b.Detect(context.Background())
	if cap.Available {
		t.Error("missing whisper-cli should be unavailable")
	}
	if cap.Detail == "" {
		t.Error("unavailable whisper-cpp must carry a Detail")
	}
}

// TestWhisperCppDetectAutoDownload: with no local binary, availability now tracks
// tools.auto_download AND whether the pinned release publishes an asset for this
// platform. Written to hold on both darwin/arm64 (a metal asset exists) and
// linux/amd64 CI (a cpu asset exists) as well as any unsupported platform.
func TestWhisperCppDetectAutoDownload(t *testing.T) {
	nope := filepath.Join(t.TempDir(), "nope") // explicit-but-missing: no PATH fallback
	device := detectWhisperDevice()
	_, supported := toolfetch.WhisperCLIAssetFor(runtime.GOOS, runtime.GOARCH, device)

	// auto-download ON, no binary.
	on := newWhisperCpp(SelectConfig{AutoDownload: true, WhisperCLIPath: nope, DataDir: t.TempDir(), Log: discardLogger()})
	cap, _ := on.Detect(context.Background())
	if supported {
		if !cap.Available {
			t.Errorf("supported platform with auto-download should be available: %+v", cap)
		}
		if cap.Version != "" {
			t.Errorf("pre-download availability must not claim a Version, got %q", cap.Version)
		}
		if !strings.Contains(cap.Detail, "downloaded on first use") {
			t.Errorf("Detail should explain the pending download, got %q", cap.Detail)
		}
	} else if cap.Available {
		t.Error("unsupported platform must not be available via auto-download")
	}

	// auto-download OFF, no binary: always unavailable with a clear Detail.
	off := newWhisperCpp(SelectConfig{AutoDownload: false, WhisperCLIPath: nope, DataDir: t.TempDir(), Log: discardLogger()})
	capOff, _ := off.Detect(context.Background())
	if capOff.Available {
		t.Error("auto-download off with no binary must be unavailable")
	}
	if capOff.Detail == "" {
		t.Error("unavailable whisper-cpp must carry a Detail")
	}
}

// TestWhisperCppEnsureReadyAutoDownloadsBinary drives EnsureReady's ordering through
// the injectable ensureWhisperCLI seam (no real network): the binary is fetched into
// the cache, then found by cliPath so Transcribe can use it. The model step is a
// no-op via a pre-seeded model + sidecar.
func TestWhisperCppEnsureReadyAutoDownloadsBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is unix-only")
	}
	dataDir := t.TempDir()
	b := newWhisperCpp(SelectConfig{AutoDownload: true, WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: dataDir, Log: discardLogger()})

	// Pre-seed the ggml model + its sidecar so EnsureModel is a cache hit (no HTTP).
	model := b.modelPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(model), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("ggml"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".meta", []byte(`{"size":4,"url":"seed"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Before EnsureReady there is no binary.
	if b.cliPath() != "" {
		t.Fatal("precondition: no whisper-cli should resolve yet")
	}

	var called bool
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(_ context.Context, toolsDir, _ string, _ *slog.Logger) (string, error) {
		called = true
		dir := filepath.Join(toolsDir, "whisper-cpp")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		p := filepath.Join(dir, "whisper-cli")
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub
			return "", err
		}
		return p, nil
	}
	defer func() { ensureWhisperCLI = restore }()

	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if !called {
		t.Error("EnsureReady should auto-download the binary when none resolves")
	}
	// The downloaded binary is now resolvable (via the toolfetch cache) so Transcribe
	// will find it.
	if got := b.cliPath(); got == "" {
		t.Error("cliPath should resolve the auto-downloaded binary after EnsureReady")
	}
}

// TestWhisperCppEnsureReadyNoAutoDownload: with auto-download off and no binary,
// EnsureReady fails clearly and never invokes the downloader.
func TestWhisperCppEnsureReadyNoAutoDownload(t *testing.T) {
	b := newWhisperCpp(SelectConfig{AutoDownload: false, WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: t.TempDir(), Log: discardLogger()})
	var called bool
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(context.Context, string, string, *slog.Logger) (string, error) {
		called = true
		return "", nil
	}
	defer func() { ensureWhisperCLI = restore }()

	if err := b.EnsureReady(context.Background(), b.dataDir); err == nil {
		t.Fatal("EnsureReady should fail without a binary and auto-download off")
	}
	if called {
		t.Error("auto-download off must not invoke the downloader")
	}
}

// TestWhisperCppEnsureReadyDownloadErrorPropagates: an ensure error surfaces as an
// EnsureReady error (the book then parks, not silently proceeds). toolfetch itself
// degrades a failed refresh to a still-present cached binary, so by contract an
// error from it means nothing is usable.
func TestWhisperCppEnsureReadyDownloadErrorPropagates(t *testing.T) {
	b := newWhisperCpp(SelectConfig{AutoDownload: true, WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: t.TempDir(), Log: discardLogger()})
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(context.Context, string, string, *slog.Logger) (string, error) {
		return "", errors.New("boom")
	}
	defer func() { ensureWhisperCLI = restore }()
	if err := b.EnsureReady(context.Background(), b.dataDir); err == nil {
		t.Fatal("a download error must propagate from EnsureReady")
	}
}

// seedWhisperCache pre-populates the managed auto-download cache with an executable
// whisper-cli stub under <dataDir>/tools/whisper-cpp and returns its path.
func seedWhisperCache(t *testing.T, dataDir string) string {
	t.Helper()
	return writeScript(t, filepath.Join(toolsDir(dataDir), "whisper-cpp", "whisper-cli"), "#!/bin/sh\nexit 0\n")
}

// seedWhisperModel pre-seeds the ggml model + its sidecar so EnsureModel is a cache
// hit (no HTTP) for the given backend.
func seedWhisperModel(t *testing.T, b *whisperCpp, dataDir string) {
	t.Helper()
	model := b.modelPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(model), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("ggml"), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".meta", []byte(`{"size":4,"url":"seed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWhisperCppEnsureReadyRefreshesExistingCache: with auto-download on and NO
// user-owned local install, EnsureReady must invoke ensureWhisperCLI even though a
// cached binary already resolves - that call's .meta-gated cache-hit path is the
// ONLY place a tag bump (upgrade) or a meta-less partial install is repaired, so
// skipping it whenever cliPath() resolved would strand the cache on the old tag
// forever (the QA-found defect). The re-download-on-tag-mismatch behavior itself is
// covered end to end in toolfetch's TestEnsureWhisperCLITagUpgrade (httptest hits
// counted); this test pins the asr-side contract that the call always happens.
func TestWhisperCppEnsureReadyRefreshesExistingCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is unix-only")
	}
	dataDir := t.TempDir()
	b := newWhisperCpp(SelectConfig{AutoDownload: true, WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: dataDir, Log: discardLogger()})
	seedWhisperCache(t, dataDir)
	seedWhisperModel(t, b, dataDir)

	// Precondition: the cached binary already resolves - the old (buggy) gate
	// "cliPath() == \"\"" would have skipped the refresh entirely.
	if b.cliPath() == "" {
		t.Fatal("precondition: the seeded cache binary should resolve")
	}

	var called bool
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(context.Context, string, string, *slog.Logger) (string, error) {
		called = true
		return "", nil
	}
	defer func() { ensureWhisperCLI = restore }()

	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	if !called {
		t.Error("EnsureReady must run ensureWhisperCLI even when a cached binary resolves (tag upgrades never fire otherwise)")
	}
}

// (The offline/stale-cache degrade lives in toolfetch now - EnsureWhisperCLI
// itself falls back to a previously-installed binary when a refresh fails, tested
// by toolfetch's TestEnsureWhisperCLIOfflineKeepsStaleCache - so an error from the
// ensure seam here means nothing is usable and must propagate; see
// TestWhisperCppEnsureReadyDownloadErrorPropagates.)

// TestWhisperCppEnsureReadyNoAutoDownloadKeepsCache: with auto-download off, a
// pre-existing cache keeps working (no downloader call, no error).
func TestWhisperCppEnsureReadyNoAutoDownloadKeepsCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is unix-only")
	}
	dataDir := t.TempDir()
	b := newWhisperCpp(SelectConfig{AutoDownload: false, WhisperCLIPath: filepath.Join(t.TempDir(), "nope"), DataDir: dataDir, Log: discardLogger()})
	seedWhisperCache(t, dataDir)
	seedWhisperModel(t, b, dataDir)

	var called bool
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(context.Context, string, string, *slog.Logger) (string, error) {
		called = true
		return "", nil
	}
	defer func() { ensureWhisperCLI = restore }()

	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady with a cache and auto-download off: %v", err)
	}
	if called {
		t.Error("auto-download off must never invoke the downloader")
	}
}

// TestWhisperCppEnsureReadyLocalInstallNeverManaged: a user-owned local install
// (explicit path) is used as-is - the managed-cache refresh must NOT run.
func TestWhisperCppEnsureReadyLocalInstallNeverManaged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is unix-only")
	}
	dataDir := t.TempDir()
	cli := fakeWhisperCLI(t)
	b := newWhisperCpp(SelectConfig{AutoDownload: true, WhisperCLIPath: cli, DataDir: dataDir, Log: discardLogger()})
	seedWhisperModel(t, b, dataDir)

	var called bool
	restore := ensureWhisperCLI
	ensureWhisperCLI = func(context.Context, string, string, *slog.Logger) (string, error) {
		called = true
		return "", nil
	}
	defer func() { ensureWhisperCLI = restore }()

	if err := b.EnsureReady(context.Background(), dataDir); err != nil {
		t.Fatalf("EnsureReady with a local install: %v", err)
	}
	if called {
		t.Error("a user-owned local install must never trigger the managed-cache refresh")
	}
}

func TestSelect(t *testing.T) {
	// Unknown backend errors.
	if _, _, err := Select(context.Background(), SelectConfig{Backend: "bogus"}); err == nil {
		t.Error("unknown backend should error")
	}
	// Explicit whisper-cpp with a fake cli is available on any unix.
	if runtime.GOOS != "windows" {
		cli := fakeWhisperCLI(t)
		b, cap, err := Select(context.Background(), SelectConfig{Backend: "whisper-cpp", WhisperCLIPath: cli, DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Select whisper-cpp: %v", err)
		}
		if b.ID() != IDWhisperCpp || !cap.Available {
			t.Errorf("whisper-cpp select = %s / available=%v", b.ID(), cap.Available)
		}
	}
	// auto on darwin/arm64 with python3 present picks mlx-whisper.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		b, cap, err := Select(context.Background(), SelectConfig{Backend: "auto", DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Select auto: %v", err)
		}
		// python3 is present in this env; expect mlx and availability.
		if b.ID() != IDMLXWhisper {
			t.Errorf("auto picked %s, want mlx-whisper", b.ID())
		}
		_ = cap
	}
	// auto with a fake whisper-cli and no mlx (non-darwin) picks whisper-cpp.
	if runtime.GOOS != "darwin" {
		cli := fakeWhisperCLI(t)
		b, cap, err := Select(context.Background(), SelectConfig{Backend: "auto", WhisperCLIPath: cli, DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Select auto non-darwin: %v", err)
		}
		if b.ID() != IDWhisperCpp || !cap.Available {
			t.Errorf("auto non-darwin = %s / available=%v, want whisper-cpp/available", b.ID(), cap.Available)
		}
	}
}
