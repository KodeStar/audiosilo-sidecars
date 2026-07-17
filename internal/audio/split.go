package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// minFlacBytes is the size below which an existing chapter FLAC is treated as
// implausible (a degenerate/empty file) and re-split. The real guarantee against a
// half-written file is the temp-file + atomic-rename below - a chapter FLAC that
// exists at its final path is always complete - so this is a cheap secondary check.
const minFlacBytes = 256

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
	if report != nil {
		report(0, total)
	}
	for i, ch := range m.Chapters {
		if err := ctx.Err(); err != nil {
			return err // clean pause/cancel/shutdown; completed chapters remain
		}
		out := filepath.Join(chaptersDir, ChapterFileName(ch.Chapter))
		if complete(out, ch.Duration) {
			if report != nil {
				report(i+1, total)
			}
			continue
		}
		if err := splitChapter(ctx, m, ch, ffmpegPath, out); err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // killed by cancellation, not a real failure
			}
			return err
		}
		if report != nil {
			report(i+1, total)
		}
	}
	return nil
}

// splitChapter converts one chapter to a FLAC at out, via a temp file + rename.
func splitChapter(ctx context.Context, m Manifest, ch Chapter, ffmpegPath, out string) error {
	var inputArgs []string
	if m.Style == StyleFiles {
		// One whole file becomes one chapter.
		inputArgs = []string{"-i", ch.FilePath}
	} else {
		// Seek within the single book file (input seeking: -ss before -i).
		inputArgs = []string{"-ss", ftoa(ch.Start), "-i", m.Source, "-t", ftoa(ch.Duration)}
	}
	return encodeFLAC(ctx, ffmpegPath, inputArgs, out, fmt.Sprintf("chapter %d", ch.Chapter))
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
