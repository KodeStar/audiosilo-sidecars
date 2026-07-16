package asr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/toolfetch"
)

// whisperCLIName is the whisper.cpp CLI binary this backend resolves/invokes.
const whisperCLIName = "whisper-cli"

// modelsSubdir is where the ggml model is cached under <data>/tools.
const modelsSubdir = "models"

// whisperCppModelURL is the pinned Hugging Face source for the default model
// (~1.6 GiB). Both the model AND (when auto-download is enabled) the whisper-cli
// binary are fetched: the model from here, the binary from the pinned release via
// toolfetch.EnsureWhisperCLI.
const whisperCppModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin"

// minWhisperModelBytes is the floor a downloaded model must exceed to be trusted
// (the real file is ~1.6 GiB; this rejects a truncated download or an HTML error
// page). Kept well below the true size for headroom.
const minWhisperModelBytes = 1 << 30 // 1 GiB

// ensureWhisperCLI is the whisper-cli auto-download entry point, a package var so a
// test can substitute it (mirrors toolfetch's platformSpec seam) and exercise
// EnsureReady's ordering without a real network fetch. Production always uses the
// real toolfetch implementation.
var ensureWhisperCLI = toolfetch.EnsureWhisperCLI

// whisperCpp is the cross-platform ASR backend over a resolved whisper-cli binary
// and a downloaded ggml model.
type whisperCpp struct {
	cliExplicit  string // configured whisper-cli path ("" resolves automatically)
	modelName    string // ggml model filename (default DefaultWhisperCppModel)
	language     string
	autoDownload bool // fetch a prebuilt whisper-cli when none resolves locally
	dataDir      string
	log          *slog.Logger
}

func newWhisperCpp(cfg SelectConfig) *whisperCpp {
	model := cfg.Model
	if model == "" {
		model = DefaultWhisperCppModel
	}
	lang := cfg.Language
	if lang == "" {
		lang = DefaultLanguage
	}
	return &whisperCpp{
		cliExplicit:  cfg.WhisperCLIPath,
		modelName:    model,
		language:     lang,
		autoDownload: cfg.AutoDownload,
		dataDir:      cfg.DataDir,
		log:          orDiscard(cfg.Log),
	}
}

func (w *whisperCpp) ID() string { return IDWhisperCpp }

// localCLIPath resolves a user-owned LOCAL whisper-cli install (explicit config
// path -> beside the daemon binary -> $PATH), or "" when none exists. A local
// install is used as-is and never managed; everything else goes through the
// toolfetch auto-download cache. This is the single local-vs-managed distinction
// cliPath and EnsureReady share.
func (w *whisperCpp) localCLIPath() string {
	return toolfetch.LocateBinary(whisperCLIName, w.cliExplicit)
}

// cliPath resolves the whisper-cli binary to invoke: the local install, else the
// toolfetch auto-download cache under <data>/tools/whisper-cpp, else "". A binary
// EnsureReady auto-downloaded lands in the cache, so a later Transcribe finds it
// here without re-resolving.
func (w *whisperCpp) cliPath() string {
	if p := w.localCLIPath(); p != "" {
		return p
	}
	return toolfetch.CachedWhisperCLI(toolsDir(w.dataDir))
}

// modelPath is the cached model file location under <data>/tools/models.
func (w *whisperCpp) modelPath(dataDir string) string {
	return filepath.Join(toolsDir(dataDir), modelsSubdir, w.modelName)
}

// Detect reports availability and the informational device. It is available when a
// whisper-cli binary resolves locally OR when auto-download is on and the pinned
// release publishes a binary for this platform+device (fetched lazily by
// EnsureReady). It performs no network I/O and does not download the model.
func (w *whisperCpp) Detect(_ context.Context) (Capability, error) {
	device := detectWhisperDevice()
	cap := Capability{Backend: IDWhisperCpp, Device: device}
	if cli := w.cliPath(); cli != "" {
		cap.Available = true
		cap.Version = "whisper.cpp (" + cli + ")"
		return cap, nil
	}
	if w.autoDownload {
		if asset, ok := toolfetch.WhisperCLIAssetFor(runtime.GOOS, runtime.GOARCH, device); ok {
			// A binary will be fetched on first use; Version stays empty until it is.
			cap.Available = true
			cap.Detail = "whisper-cli will be downloaded on first use (" + asset + ")"
			return cap, nil
		}
	}
	cap.Detail = "whisper-cli not found (explicit path, beside the binary, or PATH); install whisper.cpp, or enable tools.auto_download on a supported platform (Apple Silicon, Linux x86-64/arm64, or Windows x86-64)"
	return cap, nil
}

// EnsureReady makes the backend runnable: it settles the whisper-cli binary, then
// downloads the ggml model if it is missing. Both steps are idempotent + logged.
//
// Binary policy: a LOCAL install (localCLIPath) is the user's own - used as-is,
// never touched. Otherwise the auto-download cache is OURS to manage: when
// auto-download is on, ensureWhisperCLI ALWAYS runs, even if a cached binary
// already resolves - its cache-hit path is a cheap .meta read with zero network
// I/O when the pinned tag matches, and it is the only place a tag bump (upgrade)
// or a meta-less partial install gets repaired; gating it on "no binary resolves"
// would leave a stale cache in place forever. toolfetch itself degrades a failed
// refresh to the previously-installed cached binary, so an error from it means
// nothing is usable. With auto-download off, a pre-existing cache keeps working.
func (w *whisperCpp) EnsureReady(ctx context.Context, dataDir string) error {
	if w.localCLIPath() == "" {
		// No user-owned local install: resolve through the managed cache.
		if w.autoDownload {
			if _, err := ensureWhisperCLI(ctx, toolsDir(dataDir), detectWhisperDevice(), w.log); err != nil {
				return fmt.Errorf("whisper-cli download: %w", err)
			}
		}
		// A successful ensure and a pre-existing cache both resolve here (the local
		// half is already known empty, so only the cache needs checking); a bare miss
		// (auto-download off and nothing cached) is the clear install-or-enable error.
		if toolfetch.CachedWhisperCLI(toolsDir(dataDir)) == "" {
			return fmt.Errorf("whisper-cli not found; install whisper.cpp or enable tools.auto_download to enable the %s backend", IDWhisperCpp)
		}
	}
	dest := w.modelPath(dataDir)
	if _, err := toolfetch.EnsureModel(ctx, whisperCppModelURL, dest, minWhisperModelBytes, w.log); err != nil {
		return fmt.Errorf("whisper.cpp model: %w", err)
	}
	return nil
}

// Transcribe runs one chapter FLAC through whisper-cli, writing full JSON (segments
// + token timestamps) into job.OutDir as <flac-stem>.json. The prompt is passed
// only when non-empty (verified spellings).
func (w *whisperCpp) Transcribe(ctx context.Context, job Job) error {
	cli := w.cliPath()
	if cli == "" {
		return fmt.Errorf("whisper-cli not found; cannot transcribe")
	}
	model := w.modelPath(w.dataDir)
	if !fsutil.IsFile(model) {
		return fmt.Errorf("whisper.cpp model not present (%s); run EnsureReady", model)
	}
	lang := job.Language
	if lang == "" {
		lang = w.language
	}
	if err := os.MkdirAll(job.OutDir, 0o750); err != nil {
		return err
	}
	// -of sets the output prefix; whisper-cli appends .json. The stem matches the
	// input FLAC stem so the raw file lines up with the chapter (chNNN.json).
	outPrefix := filepath.Join(job.OutDir, strings.TrimSuffix(filepath.Base(job.Audio), filepath.Ext(job.Audio)))
	args := []string{
		"-m", model,
		"-f", job.Audio,
		"-l", lang,
		"-oj",  // JSON output
		"-ojf", // full JSON (per-token timestamps + probabilities)
		"-of", outPrefix,
	}
	if strings.TrimSpace(job.InitialPrompt) != "" {
		args = append(args, "--prompt", job.InitialPrompt)
	}
	if out, err := runTool(ctx, 2*time.Hour, cli, args...); err != nil {
		return fmt.Errorf("whisper-cli chapter %d: %w: %s", job.Chapter, err, out)
	}
	return nil
}

// detectWhisperDevice guesses the informational accelerator: Metal on Apple
// Silicon, CUDA when nvidia-smi is present, Vulkan when vulkaninfo is present,
// else CPU. This is diagnostic only in M3a (device is not a real control knob).
func detectWhisperDevice() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return DeviceMetal
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return DeviceCUDA
	}
	if _, err := exec.LookPath("vulkaninfo"); err == nil {
		return DeviceVulkan
	}
	return DeviceCPU
}
