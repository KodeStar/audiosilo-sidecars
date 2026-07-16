package toolfetch

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeTool writes an executable stub named tool into dir and returns its path.
func fakeTool(t *testing.T, dir, tool string) string {
	t.Helper()
	p := filepath.Join(dir, binName(tool))
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	return p
}

func TestSpecFor(t *testing.T) {
	for _, p := range []struct{ os, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		if _, ok := specFor(p.os, p.arch); !ok {
			t.Errorf("specFor(%s/%s) ok=false, want a source", p.os, p.arch)
		}
	}
	if _, ok := specFor("plan9", "mips"); ok {
		t.Error("specFor(unsupported) ok=true, want false")
	}
}

func TestCached(t *testing.T) {
	dir := t.TempDir()
	if fm, fp := Cached(dir); fm != "" || fp != "" {
		t.Fatalf("empty dir: Cached=%q,%q want empty", fm, fp)
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		fakeTool(t, dir, tool)
	}
	fm, fp := Cached(dir)
	if fm == "" || fp == "" {
		t.Fatalf("Cached after writing tools = %q,%q want both set", fm, fp)
	}
}

// Ensure must short-circuit (no download) when both tools are already cached AND
// pass their self-check. The stubs here `exit 0` on `-version`, so they verify.
func TestEnsureUsesCache(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		fakeTool(t, dir, tool)
	}
	fm, fp := Ensure(context.Background(), dir, discard())
	if fm != filepath.Join(dir, binName("ffmpeg")) || fp != filepath.Join(dir, binName("ffprobe")) {
		t.Fatalf("Ensure with cache = %q,%q want the cached paths", fm, fp)
	}
}

// resolveLocal must honor an explicit path, and reject an explicit-but-missing one
// WITHOUT falling back to $PATH.
func TestResolveLocalExplicit(t *testing.T) {
	dir := t.TempDir()
	ff := fakeTool(t, dir, "ffmpeg")
	if got := resolveLocal("ffmpeg", ff); got != ff {
		t.Errorf("explicit path: got %q want %q", got, ff)
	}
	if got := resolveLocal("ffmpeg", filepath.Join(dir, "nope")); got != "" {
		t.Errorf("missing explicit path: got %q want empty", got)
	}
}

// resolveLocal falls back to $PATH by the tool's own name when no explicit path is
// given (the daemon-adjacent copy is absent in a test binary's dir).
func TestResolveLocalPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses a shell script")
	}
	dir := t.TempDir()
	want := fakeTool(t, dir, "ffprobe")
	t.Setenv("PATH", dir)
	if got := resolveLocal("ffprobe", ""); got != want {
		t.Errorf("PATH resolution: got %q want %q", got, want)
	}
}

// Resolve with auto-download off and no local tools yields empty paths (a graceful
// degrade), and must not attempt a network fetch.
func TestResolveNoAutoDownload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // empty dir: nothing resolvable
	tools := Resolve(context.Background(), ResolveConfig{AutoDownload: false}, dir, discard())
	if tools.FFmpeg != "" || tools.FFprobe != "" {
		t.Errorf("Resolve(auto_download=false) = %+v, want empty", tools)
	}
}

// Resolve prefers explicit configured paths over everything else.
func TestResolveExplicit(t *testing.T) {
	dir := t.TempDir()
	ff := fakeTool(t, dir, "ffmpeg")
	fp := fakeTool(t, dir, "ffprobe")
	tools := Resolve(context.Background(), ResolveConfig{FFmpegPath: ff, FFprobePath: fp}, t.TempDir(), discard())
	if tools.FFmpeg != ff || tools.FFprobe != fp {
		t.Errorf("Resolve explicit = %+v, want %q,%q", tools, ff, fp)
	}
}

// extractZip must pull only ffmpeg/ffprobe (by basename, from any subdir) and mark
// them executable, ignoring everything else - and never escape destDir.
func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"ffmpeg-build/bin/" + binName("ffmpeg"):  "FFMPEG",
		"ffmpeg-build/bin/" + binName("ffprobe"): "FFPROBE",
		"ffmpeg-build/README.txt":                "ignore me",
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractZip(&buf, dir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	for tool, want := range map[string]string{"ffmpeg": "FFMPEG", "ffprobe": "FFPROBE"} {
		p := filepath.Join(dir, binName(tool))
		got, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatalf("expected %s extracted: %v", tool, err)
		}
		if string(got) != want {
			t.Errorf("%s body = %q, want %q", tool, got, want)
		}
		if info, _ := os.Stat(p); info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable (mode %v)", tool, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Error("extractZip wrote a non-tool file")
	}
}
