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
		if complete(out) {
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
	tmp := out + ".part"
	_ = os.Remove(tmp) // clear any stale partial from a prior interrupted run

	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if m.Style == StyleFiles {
		// One whole file becomes one chapter.
		args = append(args, "-i", ch.FilePath)
	} else {
		// Seek within the single book file (input seeking: -ss before -i).
		args = append(args,
			"-ss", ftoa(ch.Start),
			"-i", m.Source,
			"-t", ftoa(ch.Duration),
		)
	}
	// -f flac is required because the temp output has a .part extension, from which
	// ffmpeg cannot infer the muxer.
	args = append(args, "-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "flac", "-f", "flac", tmp)

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg chapter %d: %w: %s", ch.Chapter, err, strings.TrimSpace(stderr.String()))
	}
	return os.Rename(tmp, out)
}

// complete reports whether a chapter FLAC already exists at a plausible size.
func complete(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() >= minFlacBytes
}

// ftoa formats a seconds value with millisecond precision for ffmpeg -ss/-t.
func ftoa(sec float64) string {
	return strconv.FormatFloat(sec, 'f', 3, 64)
}
