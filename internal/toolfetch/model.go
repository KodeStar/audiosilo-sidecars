package toolfetch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxModelBytes caps a model download (defends against a runaway/HTML-error body).
// A ggml-large-v3-turbo is ~1.6 GiB, so 5 GiB is a comfortable ceiling. It is a
// var so tests can shrink it. Model files are far larger than the tool binaries,
// so they use their own cap rather than maxToolBytes.
var maxModelBytes int64 = 5 << 30 // 5 GiB

// LocateBinary resolves an external binary WITHOUT downloading it, using the same
// order as the ffmpeg/ffprobe resolution: an explicit path/name (honored exactly),
// then a copy next to the daemon binary, then $PATH. It returns "" when none is
// found. This is the shared lookup the ASR whisper.cpp backend reuses to find
// whisper-cli. (Auto-downloading the whisper.cpp binary itself is deferred to a
// later milestone's CI-built matrix; a missing binary means that backend is
// unavailable, surfaced with a clear message.)
func LocateBinary(name, explicit string) string {
	return resolveLocal(name, explicit)
}

// EnsureModel returns a usable local path to the model at url, downloading it into
// destPath if it is absent or smaller than minBytes (a truncated download or an
// error page rather than the real artifact). The download streams to a sibling
// temp file, is verified to meet the size floor, and is atomically renamed into
// place, so a process killed mid-download never leaves a half-file that a later run
// would trust. url must be https. It is idempotent: an already-present, big-enough
// file is returned immediately with no network call.
func EnsureModel(ctx context.Context, url, destPath string, minBytes int64, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return "", fmt.Errorf("model url must be https: %q", url)
	}
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() && info.Size() >= minBytes {
		return destPath, nil // already cached and plausibly whole
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return "", err
	}
	log.Info("downloading ASR model (one time)", "url", url, "into", destPath)
	if err := downloadModel(ctx, url, destPath, minBytes); err != nil {
		return "", err
	}
	log.Info("ASR model ready", "path", destPath)
	return destPath, nil
}

// downloadModel streams url into a temp file beside destPath, enforces the size
// floor and cap, and renames it into place on success.
func downloadModel(ctx context.Context, url, destPath string, minBytes int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".partial-"+filepath.Base(destPath)+"-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxModelBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	if n > maxModelBytes {
		return fmt.Errorf("model %s exceeds %d bytes", url, maxModelBytes)
	}
	if n < minBytes {
		return fmt.Errorf("model %s is only %d bytes (< %d floor); likely truncated or an error page", url, n, minBytes)
	}
	return os.Rename(tmpName, destPath)
}
