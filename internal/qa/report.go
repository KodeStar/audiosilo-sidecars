package qa

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// WriteReport writes both qa_report.json (pretty JSON, trailing newline) and
// qa_report.md into workDir, each atomically via fsutil. The markdown mirrors
// qa_sweep.py's four sections verbatim (so a later golden test can compare them to a
// historical qa_report.md) and appends sections for the cross-segment, within-segment,
// multi-loop and tail-rate detectors (which the Python printed to stdout), one line
// per finding in the same shape as their print statements. It never touches any
// transcript layer.
func WriteReport(workDir string, r *Report) error {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(workDir, ReportJSONName), append(out, '\n'), 0o644); err != nil {
		return err
	}
	md := renderMarkdown(r)
	return fsutil.WriteFileAtomic(filepath.Join(workDir, ReportMDName), []byte(md), 0o644)
}

// renderMarkdown builds the qa_report.md body (with a trailing newline).
func renderMarkdown(r *Report) string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	// Header + wph stats (qa_sweep.py verbatim).
	add("# QA report")
	add("")
	add(fmt.Sprintf("chapters: %d", r.Chapters))
	add(fmt.Sprintf("wph mean %.0f sd %.0f", r.WPHMean, r.WPHStdDev))
	add("")

	// ## wph outliers
	add("## wph outliers (|z| > 2.5)")
	if len(r.WPHOutliers) == 0 {
		add("- none")
	} else {
		for _, o := range r.WPHOutliers {
			add(fmt.Sprintf("- ch%03d: %d words, %.1f min, wph %.0f (z %+.1f)",
				o.Chapter, o.Words, o.Minutes, o.WPH, o.Z))
		}
	}
	add("")

	// ## repeated-segment runs
	add("## repeated-segment runs (>=3 identical normalized segments)")
	if len(r.RepeatedRuns) == 0 {
		add("- none")
	} else {
		for _, run := range r.RepeatedRuns {
			add(fmt.Sprintf("- ch%03d %s: %dx at %.1f min: %s",
				run.Chapter, kindLabel(run.Kind), run.Length, run.StartSec/60, PyRepr(run.Snippet)))
		}
	}
	add("")

	// ## low-confidence words
	add("## low-confidence words (<0.5)")
	lc := r.LowConfidence
	add(fmt.Sprintf("- total %d/%d (%.2f%%)", lc.TotalLow, lc.TotalWords, pct(lc.TotalLow, lc.TotalWords)))
	for _, c := range lc.Worst {
		add(fmt.Sprintf("- ch%03d: %d/%d (%.2f%%)", c.Chapter, c.Low, c.Total, pct(c.Low, c.Total)))
	}
	add("")

	// ## retranscribe queue (qa_sweep.py prints the Python list literal, or "empty").
	add("## retranscribe queue")
	add("- " + queueString(r.RetranscribeQueue))
	add("")

	// New sections (the standalone scripts' stdout, one line per finding).
	add("## cross-segment loops (6-gram repeated >=5x per chapter)")
	if len(r.CrossSegment) == 0 {
		add("- none")
	} else {
		for _, h := range r.CrossSegment {
			span := "?"
			if h.FirstSec != nil && h.LastSec != nil {
				span = fmt.Sprintf("%.0f-%.0fs", *h.FirstSec, *h.LastSec)
			}
			add(fmt.Sprintf("- ch%03d: x%3d at %5.1f%% of chapter (%s): %s",
				h.Chapter, h.Count, h.Pos, span, PyRepr(TruncateRunes(h.Phrase, snippetLen))))
		}
	}
	add("")

	add("## within-segment loops (6-gram repeated >=8x in one segment)")
	if len(r.WithinSegment) == 0 {
		add("- none")
	} else {
		for _, h := range r.WithinSegment {
			add(fmt.Sprintf("- ch%03d: 6gram x%d at %.0f%% pos: %s",
				h.Chapter, h.Count, h.Pos, PyRepr(h.Phrase)))
		}
	}
	add("")

	add("## multi-loop scan (every 6-gram repeated >=5x per chapter)")
	if len(r.MultiLoop) == 0 {
		add("- none")
	} else {
		for _, f := range r.MultiLoop {
			tag := "tail"
			if f.MidChapter {
				tag = "*** MID-CHAPTER"
			}
			where := "?"
			if f.AtSec != nil {
				where = fmt.Sprintf("%.0fs (%.0f%%)", *f.AtSec, f.Pos)
			}
			add(fmt.Sprintf("- ch%03d [%-8s] x%3d %-15s at %14s: %s",
				f.Chapter, f.Source, f.Count, tag, where, PyRepr(TruncateRunes(f.Phrase, multiPhrase))))
		}
	}
	add("")

	add(fmt.Sprintf("## tail-rate outliers (final %d words faster than %g w/s)", tailWords, tailMaxWPS))
	if len(r.TailRate) == 0 {
		add("- none")
	} else {
		for _, h := range r.TailRate {
			add(fmt.Sprintf("- ch%03d: %6.1f w/s (%d words in %ss): %s",
				h.Chapter, h.WPS, tailWords, trimFloat(h.Span), PyRepr(h.Tail)))
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

// kindLabel maps a stable RepeatedRun.Kind enum value to qa_sweep.py's historical
// display label, keeping the markdown byte-compatible with the Python report while
// the JSON contract carries the clean enum. An unknown kind renders as-is.
func kindLabel(kind string) string {
	switch kind {
	case KindEndFade:
		return "end-fade"
	case KindMidChapter:
		return "MID-CHAPTER LOOP"
	}
	return kind
}

// pct is Python's `100 * n / max(d, 1)` low-confidence percentage.
func pct(n, d int) float64 {
	return 100 * float64(n) / float64(max(d, 1))
}

// queueString renders the retranscribe queue as qa_sweep.py does: the Python list
// literal (e.g. "[2, 5, 17]") or "empty".
func queueString(q []int) string {
	if len(q) == 0 {
		return "empty"
	}
	parts := make([]string, len(q))
	for i, n := range q {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// trimFloat renders a rounded float the way Python's str() of round(x, 2) prints it:
// the shortest decimal that round-trips (e.g. 0.16, 2.5), never a fixed width.
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// PyRepr renders s the way Python's repr() of a str does, so the repeated-run
// snippet line matches qa_sweep.py exactly (a later golden test compares it against a
// historical qa_report.md). Python prefers single quotes, switches to double quotes
// only when the string contains a single quote and no double quote, escapes the
// active quote plus the standard C escapes, renders control characters as \xXX, and
// leaves printable (including non-ASCII) characters as-is.
func PyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
