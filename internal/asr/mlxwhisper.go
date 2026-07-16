package asr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// mlxVenvDir is the venv subdirectory under <data>/tools.
const mlxVenvDir = "mlx-venv"

// mlxWhisperVersion is the pinned mlx-whisper release (the validated version).
const mlxWhisperVersion = "0.4.3"

// mlxWhisperModule is the module the venv's python runs to drive the mlx-whisper
// CLI (`python -m mlxWhisperModule ...`). It is the module half of the
// `mlx_whisper` console-script entry point (`mlx_whisper.cli:main`, verified for
// mlx-whisper==0.4.3); `python -m mlx_whisper` alone does not work because the
// package ships no __main__. Running via python avoids the console script's
// unquoted-shebang breakage when <data> holds a space.
const mlxWhisperModule = "mlx_whisper.cli"

// versionMarkerName is the file inside the venv recording which mlxWhisperVersion
// was installed, so a later run can detect a pin change and reinstall in place.
const versionMarkerName = ".asr-version"

// mlxWhisper is the Apple-Silicon ASR backend. It manages a pinned Python venv and
// invokes mlx-whisper through the venv's python BINARY (never the generated console
// scripts): a console script's shebang embeds the interpreter path unquoted, so it
// fails when <data> contains a space, whereas invoking python directly with
// `-m pip` / `-m mlx_whisper.cli` dereferences no shebang. The model downloads
// itself from Hugging Face on first transcription (mlx-whisper handles that), so
// EnsureReady only has to build the venv.
//
// Verified empirically on darwin/arm64 with mlx-whisper==0.4.3: the `mlx_whisper`
// console-script entry point is `mlx_whisper.cli:main`; `python -m mlx_whisper`
// fails (the package ships no __main__), while `python -m mlx_whisper.cli` runs the
// CLI - hence mlxWhisperModule below.
type mlxWhisper struct {
	model    string
	language string
	dataDir  string
	log      *slog.Logger
}

func newMLXWhisper(cfg SelectConfig) *mlxWhisper {
	model := cfg.Model
	if model == "" {
		model = DefaultMLXModel
	}
	lang := cfg.Language
	if lang == "" {
		lang = DefaultLanguage
	}
	return &mlxWhisper{model: model, language: lang, dataDir: cfg.DataDir, log: orDiscard(cfg.Log)}
}

func (m *mlxWhisper) ID() string { return IDMLXWhisper }

// venvPath is the venv directory for a given data dir.
func (m *mlxWhisper) venvPath(dataDir string) string {
	return filepath.Join(toolsDir(dataDir), mlxVenvDir)
}

// venvBin returns the path to a script inside the venv's bin directory.
func (m *mlxWhisper) venvBin(dataDir, name string) string {
	return filepath.Join(m.venvPath(dataDir), "bin", name)
}

// Detect reports availability: darwin/arm64 with python3 on PATH. It does not
// build the venv (that is EnsureReady).
func (m *mlxWhisper) Detect(_ context.Context) (Capability, error) {
	cap := Capability{Backend: IDMLXWhisper, Device: DeviceMetal}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		cap.Detail = "mlx-whisper requires macOS on Apple Silicon (darwin/arm64)"
		return cap, nil
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		cap.Detail = "python3 not found on PATH; install Python 3 to enable mlx-whisper"
		return cap, nil
	}
	cap.Available = true
	cap.Version = pythonVersion(py)
	return cap, nil
}

// EnsureReady builds the pinned venv if it is not already present, and otherwise
// enforces the pinned version on an existing venv (see ensurePinnedVersion). It is
// idempotent: a venv already at mlxWhisperVersion is left alone.
func (m *mlxWhisper) EnsureReady(ctx context.Context, dataDir string) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("mlx-whisper requires macOS on Apple Silicon (darwin/arm64)")
	}
	whisper := m.venvBin(dataDir, "mlx_whisper")
	if fsutil.IsFile(whisper) {
		// The venv exists; make sure it holds the pinned version (a bumped pin, or a
		// venv predating the marker, reinstalls in place rather than rebuilding).
		return m.ensurePinnedVersion(ctx, dataDir)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		return errors.New("python3 not found on PATH; install Python 3 to enable mlx-whisper")
	}
	venv := m.venvPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(venv), 0o750); err != nil {
		return err
	}
	m.log.Info("mlx-whisper: creating Python venv (one time)", "venv", venv)
	if out, err := runTool(ctx, 5*time.Minute, py, "-m", "venv", venv); err != nil {
		return fmt.Errorf("create venv: %w: %s", err, out)
	}
	python := m.venvBin(dataDir, "python")
	m.log.Info("mlx-whisper: installing mlx-whisper into the venv (one time)", "version", mlxWhisperVersion)
	if out, err := runTool(ctx, 15*time.Minute, python, "-m", "pip", "install", "mlx-whisper=="+mlxWhisperVersion); err != nil {
		return fmt.Errorf("pip install mlx-whisper: %w: %s", err, out)
	}
	if !fsutil.IsFile(whisper) {
		return fmt.Errorf("mlx-whisper install did not produce %s", whisper)
	}
	if err := m.writeVersionMarker(dataDir); err != nil {
		return err
	}
	m.log.Info("mlx-whisper: venv ready", "venv", venv)
	return nil
}

// ensurePinnedVersion enforces mlxWhisperVersion on an already-provisioned venv:
// when the recorded marker matches the pin it is a no-op, otherwise (a bumped pin,
// or a venv that predates the marker) it pip-installs the pinned version IN PLACE
// and rewrites the marker - it never rebuilds the whole venv, which would waste the
// expensive interpreter+deps that are unaffected by a mlx-whisper version bump.
func (m *mlxWhisper) ensurePinnedVersion(ctx context.Context, dataDir string) error {
	if m.readVersionMarker(dataDir) == mlxWhisperVersion {
		return nil
	}
	python := m.venvBin(dataDir, "python")
	m.log.Info("mlx-whisper: pinned version changed; reinstalling in venv", "version", mlxWhisperVersion)
	if out, err := runTool(ctx, 15*time.Minute, python, "-m", "pip", "install", "mlx-whisper=="+mlxWhisperVersion); err != nil {
		return fmt.Errorf("pip install mlx-whisper: %w: %s", err, out)
	}
	return m.writeVersionMarker(dataDir)
}

// versionMarkerPath is the pinned-version marker file inside the venv.
func (m *mlxWhisper) versionMarkerPath(dataDir string) string {
	return filepath.Join(m.venvPath(dataDir), versionMarkerName)
}

// readVersionMarker returns the trimmed version recorded in the venv's marker, or
// "" when it is missing/unreadable (which forces a reinstall).
func (m *mlxWhisper) readVersionMarker(dataDir string) string {
	raw, err := os.ReadFile(m.versionMarkerPath(dataDir)) //nolint:gosec // path derives from the data dir
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeVersionMarker records mlxWhisperVersion in the venv's marker file.
func (m *mlxWhisper) writeVersionMarker(dataDir string) error {
	return fsutil.WriteFileAtomic(m.versionMarkerPath(dataDir), []byte(mlxWhisperVersion+"\n"), 0o644)
}

// Transcribe runs one chapter FLAC through mlx-whisper via the venv's python
// binary (`python -m mlx_whisper.cli ...`, not the console script - see the type
// doc), writing raw JSON (with word timestamps) into job.OutDir as
// <flac-stem>.json. The initial prompt is passed only when non-empty (verified
// spellings), matching audio_extract.py - a seeded guess makes a wrong spelling
// recur.
func (m *mlxWhisper) Transcribe(ctx context.Context, job Job) error {
	python := m.venvBin(m.dataDir, "python")
	if !fsutil.IsFile(python) {
		return fmt.Errorf("mlx-whisper venv not provisioned (%s missing); run EnsureReady", python)
	}
	lang := job.Language
	if lang == "" {
		lang = m.language
	}
	if err := os.MkdirAll(job.OutDir, 0o750); err != nil {
		return err
	}
	args := []string{
		"-m", mlxWhisperModule,
		job.Audio,
		"--model", m.model,
		"--language", lang,
		"--output-dir", job.OutDir,
		"--output-format", "json",
		"--word-timestamps", "True",
		"--verbose", "False",
	}
	if strings.TrimSpace(job.InitialPrompt) != "" {
		args = append(args, "--initial-prompt", job.InitialPrompt)
	}
	if out, err := runTool(ctx, 2*time.Hour, python, args...); err != nil {
		return fmt.Errorf("mlx_whisper chapter %d: %w: %s", job.Chapter, err, out)
	}
	return nil
}

// pythonVersion returns `python3 --version` output (trimmed) for the /system
// diagnostic, or "" if it cannot be read.
func pythonVersion(py string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runTool runs a command with a timeout and returns its combined output on error
// (trimmed) so the caller can surface a diagnostic.
func runTool(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// name is a resolved tool path (the venv's python or whisper-cli) and args are
	// argv-only (no shell), so there is no injection surface here.
	cmd := exec.CommandContext(cctx, name, args...) //nolint:gosec // resolved tool path + argv-only
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}
