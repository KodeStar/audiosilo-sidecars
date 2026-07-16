package toolfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureModelDownloadsAndCaches(t *testing.T) {
	body := strings.Repeat("M", 4096) // stand-in for the real ~1.6 GiB model
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "models", "ggml-test.bin")
	url := srv.URL + "/ggml-test.bin"

	// EnsureModel builds an http.Client with a nil Transport, which falls back to
	// http.DefaultTransport. The httptest TLS server's self-signed cert is untrusted
	// by the default transport, so install the server's trusting transport for the
	// duration of the test.
	restore := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = restore }()

	got, err := EnsureModel(context.Background(), url, dest, int64(len(body)), discard())
	if err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	if got != dest {
		t.Errorf("path = %q, want %q", got, dest)
	}
	data, err := os.ReadFile(dest)
	if err != nil || len(data) != len(body) {
		t.Fatalf("downloaded file wrong: err=%v len=%d", err, len(data))
	}

	// Second call is a cache hit: no new request.
	if _, err := EnsureModel(context.Background(), url, dest, int64(len(body)), discard()); err != nil {
		t.Fatalf("EnsureModel cached: %v", err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (second call should be cached)", hits)
	}
}

func TestEnsureModelRejectsTruncated(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tiny")) // 4 bytes, below the floor
	}))
	defer srv.Close()
	restore := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = restore }()

	dir := t.TempDir()
	dest := filepath.Join(dir, "ggml.bin")
	_, err := EnsureModel(context.Background(), srv.URL+"/x", dest, 1000, discard())
	if err == nil {
		t.Fatal("expected an error for a below-floor download")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a rejected download must not leave the dest file in place")
	}
}

func TestEnsureModelRequiresHTTPS(t *testing.T) {
	dir := t.TempDir()
	_, err := EnsureModel(context.Background(), "http://example.com/x.bin", filepath.Join(dir, "x.bin"), 1, discard())
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http url should be rejected, got %v", err)
	}
}

func TestLocateBinary(t *testing.T) {
	dir := t.TempDir()
	// An explicit absolute path that exists resolves to itself.
	tool := fakeTool(t, dir, "whisper-cli")
	if got := LocateBinary("whisper-cli", tool); got != tool {
		t.Errorf("explicit path = %q, want %q", got, tool)
	}
	// An explicit-but-missing path does NOT fall back.
	if got := LocateBinary("whisper-cli", filepath.Join(dir, "nope")); got != "" {
		t.Errorf("missing explicit path = %q, want empty", got)
	}
	// No explicit + not on PATH -> empty.
	if got := LocateBinary("audiosilo-nonexistent-xyz", ""); got != "" {
		t.Errorf("unknown tool = %q, want empty", got)
	}
}
