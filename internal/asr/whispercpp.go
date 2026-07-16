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
// (~1.6 GiB). Only the model is auto-downloaded here; the whisper-cli binary is
// not (its CI-built matrix is M3b), so a missing binary means unavailable.
const whisperCppModelURL = "https://huggingface.co/ggml-org/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin"

// minWhisperModelBytes is the floor a downloaded model must exceed to be trusted
// (the real file is ~1.6 GiB; this rejects a truncated download or an HTML error
// page). Kept well below the true size for headroom.
const minWhisperModelBytes = 1 << 30 // 1 GiB

// whisperCpp is the cross-platform ASR backend over a resolved whisper-cli binary
// and a downloaded ggml model.
type whisperCpp struct {
	cliExplicit string // configured whisper-cli path ("" resolves automatically)
	modelName   string // ggml model filename (default DefaultWhisperCppModel)
	language    string
	dataDir     string
	log         *slog.Logger
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
		cliExplicit: cfg.WhisperCLIPath,
		modelName:   model,
		language:    lang,
		dataDir:     cfg.DataDir,
		log:         orDiscard(cfg.Log),
	}
}

func (w *whisperCpp) ID() string { return IDWhisperCpp }

// cliPath resolves the whisper-cli binary (explicit -> beside daemon -> PATH), or
// "" if not found.
func (w *whisperCpp) cliPath() string {
	return toolfetch.LocateBinary(whisperCLIName, w.cliExplicit)
}

// modelPath is the cached model file location under <data>/tools/models.
func (w *whisperCpp) modelPath(dataDir string) string {
	return filepath.Join(toolsDir(dataDir), modelsSubdir, w.modelName)
}

// Detect reports availability (a resolvable whisper-cli) and the informational
// device. It does not download the model (that is EnsureReady).
func (w *whisperCpp) Detect(_ context.Context) (Capability, error) {
	cap := Capability{Backend: IDWhisperCpp, Device: detectWhisperDevice()}
	cli := w.cliPath()
	if cli == "" {
		cap.Detail = "whisper-cli not found (explicit path, beside the binary, or PATH); install whisper.cpp - binary auto-download arrives in a later milestone (M3b)"
		return cap, nil
	}
	cap.Available = true
	cap.Version = "whisper.cpp (" + cli + ")"
	return cap, nil
}

// EnsureReady downloads the ggml model if it is missing (the whisper-cli binary
// must already be present - Detect gates on it). Idempotent + logged.
func (w *whisperCpp) EnsureReady(ctx context.Context, dataDir string) error {
	if w.cliPath() == "" {
		return fmt.Errorf("whisper-cli not found; install whisper.cpp to enable the %s backend", IDWhisperCpp)
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
