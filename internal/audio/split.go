package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// minFlacBytes is the size below which an existing chapter FLAC is treated as
// implausible (a degenerate/empty file) and re-split. The real guarantee against a
// half-written file is the temp-file + atomic-rename below - a chapter FLAC that
// exists at its final path is always complete - so this is a cheap secondary check.
const minFlacBytes = 256

// SplitSourceDir holds the private, per-book copy of the source used while a
// marker-style book is split, and stagedSourceName is the copy inside it. ffmpeg
// otherwise reopens the same (often SMB-mounted) M4B once per chapter; enough rapid
// random-access opens can wedge macOS's SMB client even with only one book running.
// One sequential copy makes every chapter conversion local. The complete copy is
// retained across a failed split for cheap resume and removed after all chapters
// finish.
//
// It is a DIRECTORY rather than a work-dir-root file because internal/scratch's
// reclaimable-artifact table is directory-based: a bare file would be invisible to
// Purge, HasReclaimable and the startup GC, so a failed, cancelled or parked split
// would strand a full source-sized copy on disk forever.
const (
	SplitSourceDir   = "split-source"
	stagedSourceName = "source"
)

// stagedCopyBufBytes sizes the staging copy. io.Copy's generic 32 KB chunks turn a
// multi-GB source into ~100k round trips over an SMB/FUSE mount, which is exactly the
// access pattern the staged copy exists to avoid.
const stagedCopyBufBytes = 4 << 20

// splitChapterTimeoutFloor / splitChapterTimeoutMultiple bound ONE chapter's ffmpeg
// conversion, mirroring the per-chapter decode bound the ASR stage applies
// (asrChapterDecodeBound in internal/pipeline). The failure they exist for is a WEDGED
// source mount: a read that never returns leaves ffmpeg blocked forever, and because the
// split stage's heartbeat ticker advances both heartbeat_at and progress_at while the
// subprocess is alive, neither supervisor detector can see it - the book holds its lane
// slot indefinitely with no error. Bound = max(floor, multiple x chapter duration); a
// zero/unknown duration yields the floor. 2x is deliberately generous: transcoding one
// chapter off a healthy mount runs far faster than realtime, so anything past twice the
// audio length is a stall rather than slow hardware. Vars so tests can shorten them.
var splitChapterTimeoutFloor = 10 * time.Minute
var splitChapterTimeoutMultiple = 2.0

// splitChapterBound returns the conversion deadline for a chapter of the given audio
// duration: max(floor, multiple x duration).
func splitChapterBound(chapterDurationSec float64) time.Duration {
	return max(splitChapterTimeoutFloor, time.Duration(splitChapterTimeoutMultiple*chapterDurationSec*float64(time.Second)))
}

// stagingCopyFloor / stagingCopyBytesPerMinute bound the staging copy for the same
// reason: a wedged mount can stall a read mid-copy, and the copy runs before the first
// progress report, so a stall there is even less visible than one inside ffmpeg. The
// budget is deliberately loose - 1 minute per 500 MiB is ~8.5 MB/s, well below any mount
// worth splitting from, and the floor covers small sources on a briefly slow link.
// Whichever is larger wins. Vars so tests can shorten them.
var stagingCopyFloor = 15 * time.Minute
var stagingCopyBytesPerMinute int64 = 500 << 20

// stagingCopyBound returns the copy deadline for a source of the given size.
func stagingCopyBound(sizeBytes int64) time.Duration {
	budget := time.Duration(float64(sizeBytes) / float64(stagingCopyBytesPerMinute) * float64(time.Minute))
	return max(stagingCopyFloor, budget)
}

// IsTransientSourceErr reports whether err carries the transient source-mount
// interruption shape: a FUSE/Nextcloud (or SMB) mount can return EINTR while ffmpeg
// opens the source even though the mount is healthy, and re-running the resumable
// split then succeeds. It matches on the message because the interruption reaches us
// as ffmpeg's stderr prose from a subprocess, never as a Go errno. It is deliberately
// narrow - every other ffmpeg failure keeps its normal hard-failure semantics.
func IsTransientSourceErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "interrupted system call")
}

// ProgressFunc reports split progress as chapters complete (done of total).
type ProgressFunc func(done, total int)

// ChapterFileName is the on-disk FLAC name for a chapter number (zero-padded,
// matching the historical ch%03d.flac scheme).
func ChapterFileName(chapter int) string {
	return fmt.Sprintf("ch%03d.flac", chapter)
}

// Split converts each manifest chapter into a mono/16 kHz FLAC under
// workDir/chapters. It is RESUMABLE and idempotent: a chapter whose FLAC already
// exists (at a plausible size) is skipped, so re-running after an interruption
// only does the missing chapters. Each conversion writes to a temp file and
// renames on success, so an interrupted ffmpeg never leaves a truncated FLAC that
// a later run would trust. Cancellation via ctx returns context.Canceled without a
// partial artifact, leaving the stage cleanly re-runnable.
//
// The FIRST progress report is emitted after staging, so a caller timing productive
// work from that report (internal/pipeline's split stage does, for the EWMA rate) never
// charges the learned chapters/sec with a multi-GB source copy.
func Split(ctx context.Context, m Manifest, workDir, ffmpegPath string, report ProgressFunc) error {
	if ffmpegPath == "" {
		return errors.New("ffmpeg unavailable; cannot split chapters")
	}
	if len(m.Chapters) == 0 {
		return errors.New("manifest has no chapters to split")
	}
	chaptersDir := filepath.Join(workDir, ChaptersDir)
	if err := os.MkdirAll(chaptersDir, 0o750); err != nil {
		return err
	}
	total := len(m.Chapters)
	// Progress baseline: count the chapters already split on entry (a resume) so the
	// FIRST report reflects prior work and an already-present FLAC never ticks the
	// counter. This keeps the scheduler's EWMA unit span (first..last reported done)
	// measuring only what THIS run actually split. The predicate matches the loop's
	// skip test (complete). It is counted BEFORE staging so a fully-resumed split - one
	// with no chapter left to cut - never pays for a copy of the whole source.
	completed := 0
	for _, ch := range m.Chapters {
		if complete(filepath.Join(chaptersDir, ChapterFileName(ch.Chapter)), ch.Duration) {
			completed++
		}
	}
	if m.Style == StyleMarkers && completed < total {
		staged, err := stageMarkerSource(ctx, m.Source, workDir)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err() // clean pause/cancel/shutdown, not a staging failure
		case err != nil:
			// Staging is an OPTIMIZATION, never a precondition: before it existed every book
			// split by streaming straight from the source, and that path still works. So a
			// staging failure degrades to it rather than failing the book. The failure that
			// forced this is ENOSPC - a work-dir volume smaller than the source made every
			// attempt fail identically, so Retry could never clear it, on a book that split
			// fine the week before. Nothing is masked: if the disk is genuinely full the
			// chapter writes below fail loudly on their own, and if the source is genuinely
			// unreadable ffmpeg says so with better diagnostics than a copy error.
			staged = ""
		}
		if staged != "" {
			m.Source = staged
		}
	}
	done := completed
	if report != nil {
		report(done, total)
	}
	for _, ch := range m.Chapters {
		if err := ctx.Err(); err != nil {
			return err // clean pause/cancel/shutdown; completed chapters remain
		}
		out := filepath.Join(chaptersDir, ChapterFileName(ch.Chapter))
		if complete(out, ch.Duration) {
			continue // already split (counted in the baseline); do not re-tick progress
		}
		if err := splitChapter(ctx, m, ch, ffmpegPath, out); err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // killed by cancellation, not a real failure
			}
			return err
		}
		done++
		if report != nil {
			report(done, total)
		}
	}
	// Every chapter is cut, so the staged copy has no further use. Best effort: what is
	// left behind is a registered scratch artifact (see internal/scratch), so a purge or
	// the startup GC still reclaims it. Removed unconditionally on success, which also
	// clears a copy left by an earlier interrupted run of a now fully-resumed split.
	_ = os.RemoveAll(filepath.Join(workDir, SplitSourceDir))
	return nil
}

// stageMarkerSource atomically copies one regular source file into the book's
// staged-source directory. A complete staged file from an earlier failed split is safe
// to reuse: the temp copy is never renamed until the copy and fsync both succeed.
// Non-regular or presently missing sources fall through to ffmpeg so its existing,
// more useful diagnostics remain unchanged (important for virtual/test inputs).
func stageMarkerSource(ctx context.Context, source, workDir string) (staged string, err error) {
	dir := filepath.Join(workDir, SplitSourceDir)
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	staged = filepath.Join(dir, stagedSourceName+filepath.Ext(source))
	info, serr := os.Stat(source)
	sourceUsable := serr == nil && info.Mode().IsRegular()
	// A staged file only appears at its final name after an atomic rename below (or
	// an operator performs the same validated pre-stage), so a cached one is always a
	// COMPLETE copy of whatever it was copied from - but not necessarily of what the
	// source holds NOW. A user who finds a corrupt/truncated source, replaces the file
	// and hits Retry expects the new bytes; an unconditional cache hit would split the
	// old ones forever, with nothing in the log to say why. Size is the cheap available
	// signal (the copy is byte-for-byte, so any replacement that changes the file
	// changes its size), and it costs one stat of a mount ffmpeg is about to read
	// anyway. When the source is NOT statable the cache is still trusted unreservedly:
	// letting a retry proceed while the SMB session that interrupted the prior split is
	// offline is exactly what the cache is for.
	if cached, cerr := os.Stat(staged); cerr == nil && cached.Mode().IsRegular() && cached.Size() > 0 {
		if !sourceUsable || info.Size() == cached.Size() {
			return staged, nil
		}
	}
	if !sourceUsable {
		return "", nil
	}

	tmp := staged + ".part"
	in, err := os.Open(source) //nolint:gosec // the manifest's scanned library source
	if err != nil {
		return "", err
	}
	defer in.Close()
	// O_TRUNC, not O_EXCL: a .part left by an interrupted run is stale by definition
	// (only the rename below publishes a copy), so overwriting it is the whole recovery.
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = out.Close() // a second Close after the success path is a harmless no-op
			_ = os.Remove(tmp)
		}
	}()
	// Bound the copy: a wedged mount stalls a read forever, and this runs before the
	// first progress report, so nothing downstream would ever notice. The deadline
	// reaches the read through contextReader below.
	bound := stagingCopyBound(info.Size())
	cctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	// writerOnly + an explicit buffer: io.CopyBuffer prefers dst.ReadFrom, and *os.File's
	// falls back to its own 32 KB chunks for a non-file source like contextReader.
	buf := make([]byte, stagedCopyBufBytes)
	if _, err = io.CopyBuffer(writerOnly{out}, contextReader{ctx: cctx, r: in}, buf); err != nil {
		// Name the bound when the CHILD deadline fired while the parent is alive - a stalled
		// source read, not a cancellation. (Split degrades to a direct split on any staging
		// error, so this text reaches a human through the stage log, not a park message.)
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("staging copy of %s exceeded its %s time bound: the source mount is not responding", source, bound)
		}
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(tmp, staged); err != nil {
		return "", err
	}
	return staged, nil
}

// writerOnly hides a *os.File's ReadFrom so io.CopyBuffer actually uses the buffer it
// is given instead of the generic 32 KB fallback inside os.File.ReadFrom.
type writerOnly struct{ io.Writer }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// splitChapter converts one chapter to a FLAC at out, via a temp file + rename, under
// the per-chapter time bound (splitChapterBound). A conversion that blows past the bound
// is killed and reported with a loud error naming it - deliberately NOT in the shape
// IsTransientSourceErr matches, so a wedged mount fails the stage for a human instead of
// being retried three times into the same stall.
func splitChapter(ctx context.Context, m Manifest, ch Chapter, ffmpegPath, out string) error {
	var inputArgs []string
	if m.Style == StyleFiles {
		// One whole file becomes one chapter.
		inputArgs = []string{"-i", ch.FilePath}
	} else {
		// Seek within the single book file (input seeking: -ss before -i).
		inputArgs = []string{"-ss", ftoa(ch.Start), "-i", m.Source, "-t", ftoa(ch.Duration)}
	}
	bound := splitChapterBound(ch.Duration)
	cctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	err := encodeFLAC(cctx, ffmpegPath, inputArgs, out, fmt.Sprintf("chapter %d", ch.Chapter))
	// The CHILD deadline fired while the parent is alive: a stalled conversion, not a
	// pause/shutdown (which Split's caller reports as ctx.Err()).
	if err != nil && cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("chapter %d exceeded its %s conversion time bound: the source read is stalled (a wedged network mount), not merely slow", ch.Chapter, bound)
	}
	return err
}

// CutClip cuts [startSec, startSec+durSec] of srcFlac into dstFlac as a mono/16 kHz
// FLAC - parameter-identical to the chapter FLACs Split produces, through the same
// atomic temp+rename encode path (encodeFLAC). Input seeking (-ss before -i) matches
// the historical tail_clip_check.py. Shared by internal/repair's clip cutter so clip
// audio stays bit-comparable to chapter audio.
func CutClip(ctx context.Context, ffmpegPath, srcFlac, dstFlac string, startSec, durSec float64) error {
	inputArgs := []string{"-ss", ftoa(startSec), "-i", srcFlac, "-t", ftoa(durSec)}
	return encodeFLAC(ctx, ffmpegPath, inputArgs, dstFlac, "clip")
}

// flacEncodeTail is the shared ffmpeg output selection: mono/16 kHz FLAC with the
// muxer forced (the temp .part extension gives ffmpeg no extension to infer from).
var flacEncodeTail = []string{"-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "flac", "-f", "flac"}

// encodeFLAC runs ffmpeg with the given input args (input selection + optional
// -ss/-t seeking) and encodes to out as a mono/16 kHz FLAC via a temp file + atomic
// rename, capturing stderr for the error. label names the unit in the error message.
// It is the single ffmpeg-to-FLAC path both chapter splitting and clip cutting share.
func encodeFLAC(ctx context.Context, ffmpegPath string, inputArgs []string, out, label string) error {
	if ffmpegPath == "" {
		return fmt.Errorf("ffmpeg unavailable; cannot encode %s", label)
	}
	tmp := out + ".part"
	_ = os.Remove(tmp) // clear any stale partial from a prior interrupted run

	args := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, inputArgs...)
	args = append(args, flacEncodeTail...)
	args = append(args, tmp)

	cmd := exec.CommandContext(ctx, ffmpegPath, args...) //nolint:gosec // ffmpegPath is operator-resolved (config -> $PATH -> cache); args are fixed flags + work-dir paths
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg %s: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	return os.Rename(tmp, out)
}

// complete reports whether a chapter FLAC already exists and is plausibly whole,
// so resume can skip it. The usual signal is a size at/above minFlacBytes. A
// legitimately near-silent, sub-second chapter can encode to a FLAC below that
// floor, though, which would make every resume re-split it forever - so when the
// manifest says the chapter runs under a second, any non-empty final file counts
// as complete. The atomic temp+rename in splitChapter still guarantees a file at
// the final path is never a truncated partial.
func complete(path string, chapterDuration float64) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Size() >= minFlacBytes {
		return true
	}
	return chapterDuration < 1.0 && info.Size() > 0
}

// ftoa formats a seconds value with millisecond precision for ffmpeg -ss/-t.
func ftoa(sec float64) string {
	return strconv.FormatFloat(sec, 'f', 3, 64)
}
