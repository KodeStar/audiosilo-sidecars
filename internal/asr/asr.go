// Package asr runs local speech-to-text over a book's chapter FLACs. It abstracts
// two backends behind one interface so the pipeline is hardware-agnostic:
//
//   - mlx-whisper (darwin/arm64): the validated path, managing its own pinned
//     Python venv under <data>/tools/mlx-venv and letting mlx-whisper fetch the
//     Hugging Face model on first run.
//   - whisper-cpp (every platform): a resolved whisper-cli binary plus a ggml
//     model this package downloads into <data>/tools/models.
//
// A backend produces the RAW per-chapter output byte-for-byte; normalization into
// the audiosilo-transcript/v1 contract (and NaN sanitizing) is internal/transcript's
// job, kept separate so a backend stays a thin exec wrapper. The one hard rule the
// whole product enforces (EXTRACTION-AUDIO.md) is one ASR job at a time - Metal
// contention makes concurrent jobs slower - which the scheduler guarantees via its
// capacity-1 ASR lane; this package does not itself serialize.
package asr

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
)

// Backend IDs (also the config values and the transcript provenance strings).
const (
	IDMLXWhisper = "mlx-whisper"
	IDWhisperCpp = "whisper-cpp"
)

// Device is the informational accelerator a backend runs on. It is diagnostic in
// M3a (asr.device is not yet a real control knob).
const (
	DeviceMetal  = "metal"
	DeviceCUDA   = "cuda"
	DeviceVulkan = "vulkan"
	DeviceCPU    = "cpu"
)

// Default models for each backend when config leaves asr.model empty.
const (
	DefaultMLXModel        = "mlx-community/whisper-large-v3-turbo"
	DefaultWhisperCppModel = "ggml-large-v3-turbo.bin"
)

// DefaultLanguage is the transcription language when config leaves asr.language
// empty.
const DefaultLanguage = "en"

// DefaultModelFor returns the default model identifier a backend uses when config
// leaves asr.model empty, so a caller can record accurate provenance without
// asking the backend. An unknown backend id returns "".
func DefaultModelFor(backendID string) string {
	switch backendID {
	case IDMLXWhisper:
		return DefaultMLXModel
	case IDWhisperCpp:
		return DefaultWhisperCppModel
	default:
		return ""
	}
}

// Capability describes a backend's readiness, surfaced on /system so the operator
// can see whether ASR will run and on what device. Detail carries a human message
// when Available is false (e.g. "python3 not found").
type Capability struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
	Device    string `json:"device"`
	Version   string `json:"version"`
	Detail    string `json:"detail"`
}

// Job is one chapter transcription: the input FLAC, the directory the raw output
// is written into (the backend names the file from the FLAC stem), the chapter
// number, an optional initial prompt (VERIFIED spellings only - never seeded
// guesses, which make a wrong spelling recur), and the language.
type Job struct {
	Audio         string
	OutDir        string
	Chapter       int
	InitialPrompt string
	Language      string
}

// Backend is one ASR implementation. Detect is cheap and side-effect-free (probe
// availability + device). EnsureReady is idempotent and may be expensive (build a
// venv, download a model); it is called once before a book's chapters. Transcribe
// runs ONE chapter to a raw output file; per-chapter resume is the caller's
// concern (the pipeline skips chapters whose raw output already parses complete).
type Backend interface {
	ID() string
	Detect(ctx context.Context) (Capability, error)
	EnsureReady(ctx context.Context, dataDir string) error
	Transcribe(ctx context.Context, job Job) error
}

// SelectConfig chooses and configures a backend. Backend is "auto" (or ""),
// "mlx-whisper", or "whisper-cpp". Model/Language fall back to the backend
// defaults. WhisperCLIPath is an explicit whisper-cli location ("" resolves it).
// DataDir is the daemon data dir; backends derive <DataDir>/tools/... from it.
type SelectConfig struct {
	Backend        string
	Model          string
	Language       string
	WhisperCLIPath string
	DataDir        string
	Log            *slog.Logger
}

// Select resolves the configured (or auto-detected) backend and reports its
// capability. It never fails for an unavailable backend: it returns a non-nil
// Backend whose Transcribe surfaces a clear error, plus a Capability with
// Available=false and a Detail message, so /system can explain the state and the
// pipeline fails the book cleanly. It errors only on an unknown backend name.
//
// auto = mlx-whisper on darwin/arm64 when python3 is present, else whisper-cpp
// when a whisper-cli binary is found, else unavailable.
func Select(ctx context.Context, cfg SelectConfig) (Backend, Capability, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	switch normalizeBackend(cfg.Backend) {
	case "mlx-whisper":
		b := newMLXWhisper(cfg)
		cap, _ := b.Detect(ctx)
		return b, cap, nil
	case "whisper-cpp":
		b := newWhisperCpp(cfg)
		cap, _ := b.Detect(ctx)
		return b, cap, nil
	case "auto":
		return selectAuto(ctx, cfg)
	default:
		return nil, Capability{}, fmt.Errorf("unknown asr.backend %q (want auto, %s, or %s)", cfg.Backend, IDMLXWhisper, IDWhisperCpp)
	}
}

// selectAuto prefers mlx-whisper on Apple Silicon (with python3), then a resolved
// whisper-cli, else returns the mlx backend carrying an unavailable capability so
// /system still names a sensible default and explains why it cannot run.
func selectAuto(ctx context.Context, cfg SelectConfig) (Backend, Capability, error) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		mlx := newMLXWhisper(cfg)
		if cap, _ := mlx.Detect(ctx); cap.Available {
			return mlx, cap, nil
		}
	}
	wc := newWhisperCpp(cfg)
	if cap, _ := wc.Detect(ctx); cap.Available {
		return wc, cap, nil
	}
	// Nothing available: report the platform-appropriate default's capability so the
	// Detail explains the fix (install python3+mlx, or install whisper.cpp).
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		mlx := newMLXWhisper(cfg)
		cap, _ := mlx.Detect(ctx)
		return mlx, cap, nil
	}
	cap, _ := wc.Detect(ctx)
	return wc, cap, nil
}

// normalizeBackend maps "" to "auto" and passes through the known values.
func normalizeBackend(b string) string {
	if b == "" {
		return "auto"
	}
	return b
}

// toolsDir is the shared <data>/tools directory a backend caches its venv/model
// under, derived from the daemon data dir.
func toolsDir(dataDir string) string { return filepath.Join(dataDir, "tools") }

// orDiscard returns log, or a discard logger when log is nil, so a directly
// constructed backend never nil-panics on a log call.
func orDiscard(log *slog.Logger) *slog.Logger {
	if log != nil {
		return log
	}
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

// nopWriter is a no-op io.Writer for a discard logger (avoids importing io here).
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
