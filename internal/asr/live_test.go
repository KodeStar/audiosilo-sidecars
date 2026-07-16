//go:build asrlive

// Package asr live smoke test. Run with:
//
//	go test -tags asrlive -run TestLiveMLX ./internal/asr/
//
// It builds a FRESH mlx-whisper venv under a temp data dir (never reusing an
// existing one), downloads the model on first run via Hugging Face, and transcribes
// one tiny generated FLAC end to end. It is excluded from the normal gate because it
// needs the network, ~1.6 GiB of model, and real hardware; everything else in the
// package is covered by fakes. NEVER run two live transcriptions concurrently
// (Metal contention) - this test runs exactly one.
package asr

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

func TestLiveMLX(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dataDir := t.TempDir()
	cfg := SelectConfig{Backend: IDMLXWhisper, DataDir: dataDir, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	b, cap, err := Select(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !cap.Available {
		t.Skipf("mlx-whisper not available on this machine: %s", cap.Detail)
	}
	t.Logf("mlx-whisper available: device=%s version=%s", cap.Device, cap.Version)

	// A tiny FLAC (mono/16k) - a short tone is enough to exercise the full plumbing.
	flac := filepath.Join(dataDir, "ch001.flac")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=2",
		"-ac", "1", "-ar", "16000", "-c:a", "flac", flac).CombinedOutput(); err != nil {
		t.Fatalf("generate flac: %v: %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	start := time.Now()
	if err := b.EnsureReady(ctx, dataDir); err != nil {
		t.Fatalf("EnsureReady (venv build): %v", err)
	}
	t.Logf("venv ready in %s", time.Since(start).Round(time.Second))

	outDir := filepath.Join(dataDir, transcript.RawDir)
	tstart := time.Now()
	if err := b.Transcribe(ctx, Job{Audio: flac, OutDir: outDir, Chapter: 1, Language: "en"}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("transcribed in %s", time.Since(tstart).Round(time.Second))

	raw, err := os.ReadFile(filepath.Join(outDir, transcript.RawName(1)))
	if err != nil {
		t.Fatalf("read raw transcript: %v", err)
	}
	if !transcript.Complete(raw) {
		t.Fatalf("raw transcript is not structurally complete: %s", raw)
	}
	tr, err := transcript.Normalize(raw, transcript.Meta{Chapter: 1, Backend: b.ID(), Model: DefaultMLXModel, Language: "en"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	t.Logf("normalized: schema=%s segments=%d text=%q", tr.Schema, len(tr.Segments), transcript.PlainText(tr))
}
