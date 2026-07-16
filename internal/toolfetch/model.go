package toolfetch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxModelBytes caps a model download (defends against a runaway/HTML-error body).
// A ggml-large-v3-turbo is ~1.6 GiB, so 5 GiB is a comfortable ceiling. It is a
// var so tests can shrink it. Model files are far larger than the tool binaries,
// so they use their own cap rather than maxToolBytes.
var maxModelBytes int64 = 5 << 30 // 5 GiB

// modelMeta is the <model>.meta sidecar written after a completed download. It lets
// a cache-hit check verify the cached file is byte-for-byte the size we fetched,
// catching a truncated/corrupted cache that a bare floor check would trust forever.
type modelMeta struct {
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// metaPath returns the sidecar path for a given model destination.
func metaPath(destPath string) string { return destPath + ".meta" }

// LocateBinary resolves an external binary WITHOUT downloading it, using the same
// order as the ffmpeg/ffprobe resolution: an explicit path/name (honored exactly),
// then a copy next to the daemon binary, then $PATH. It returns "" when none is
// found. This is the shared lookup the ASR whisper.cpp backend tries first for
// whisper-cli; when it comes up empty and auto-download is on, the backend falls
// back to EnsureWhisperCLI, which fetches a prebuilt binary from the pinned release
// (see whisper.go).
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
	if err := requireHTTPS(url, "model"); err != nil {
		return "", err
	}
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() {
		if meta, ok := readJSONSidecar[modelMeta](metaPath(destPath)); ok {
			// A sidecar exists: require an exact size match. A truncated/corrupted
			// cache fails here and re-downloads rather than being trusted forever.
			if info.Size() == meta.Size {
				return destPath, nil // cached and byte-for-byte the size we fetched
			}
			log.Warn("cached ASR model size mismatch; re-downloading",
				"path", destPath, "have", info.Size(), "want", meta.Size)
		} else if info.Size() >= minBytes {
			// Legacy fallback: no sidecar (downloaded before this change), so trust
			// the size floor as before.
			return destPath, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return "", err
	}
	log.Info("downloading ASR model (one time)", "url", url, "into", destPath)
	if err := downloadModel(ctx, url, destPath, minBytes, log); err != nil {
		return "", err
	}
	if err := writeMeta(metaPath(destPath), destPath, url); err != nil {
		return "", err
	}
	log.Info("ASR model ready", "path", destPath)
	return destPath, nil
}

// writeMeta stats the freshly-downloaded destPath for its true byte count and writes
// the sidecar. A partial/corrupt sidecar only fails a later equality check and
// forces a safe re-download, so it is not correctness-critical.
func writeMeta(path, destPath, url string) error {
	info, err := os.Stat(destPath)
	if err != nil {
		return err
	}
	return writeJSONSidecar(path, modelMeta{Size: info.Size(), URL: url})
}

// progressStep is how much must download between progress log lines (~100 MiB), so
// a multi-GiB first-run fetch reports steadily instead of going silent for minutes.
const progressStep = 100 << 20

// progressWriter is an io.Writer that logs cumulative download progress every
// progressStep bytes. It counts only; the bytes flow to the real sink alongside it
// (io.MultiWriter). what names the artifact in the log line (an ASR model, a
// whisper-cli asset), so the shared progress plumbing never mislabels what it is
// fetching. Total exposes the final byte count for a completion log.
type progressWriter struct {
	log     *slog.Logger
	what    string
	Total   int64
	nextLog int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.Total += int64(len(b))
	if p.Total >= p.nextLog {
		p.log.Info("downloading "+p.what, "downloaded_mb", p.Total>>20)
		p.nextLog += progressStep
	}
	return len(b), nil
}

// downloadModel streams url into a temp file beside destPath, enforces the size
// floor and cap, logs progress as it goes, and renames it into place on success.
func downloadModel(ctx context.Context, url, destPath string, minBytes int64, log *slog.Logger) error {
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

	pw := &progressWriter{log: log, what: "ASR model", nextLog: progressStep}
	n, err := io.Copy(io.MultiWriter(tmp, pw), io.LimitReader(resp.Body, maxModelBytes+1))
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
	log.Info("ASR model download complete", "downloaded_mb", n>>20, "bytes", n)
	return os.Rename(tmpName, destPath)
}
