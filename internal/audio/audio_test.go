package audio

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/scan"
)

// --- pure marker parsing / contiguity (no ffmpeg) ---

func TestChapterFromMarker(t *testing.T) {
	cases := []struct {
		in      string
		wantNum int
		wantTit string
		wantOk  bool
	}{
		{"Chapter 1: Troll Hunt", 1, "Troll Hunt", true},  // colon style
		{"1. Troll Hunt", 1, "Troll Hunt", true},          // bare number-dot style
		{"Chapter 4. The Deep", 4, "The Deep", true},      // dot style
		{"Chapter 7 - The Hyphen", 7, "The Hyphen", true}, // hyphen style
		{"Chapter 12", 12, "", true},                      // no title
		{"chapter 3: lowercase", 3, "lowercase", true},    // case-insensitive
		{"Opening Credits", 0, "", false},                 // credits excluded
		{"End Credits", 0, "", false},
		{"Prologue", 0, "", false},
	}
	for _, c := range cases {
		num, tit, ok := chapterFromMarker(c.in)
		if ok != c.wantOk || num != c.wantNum || tit != c.wantTit {
			t.Errorf("chapterFromMarker(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.in, num, tit, ok, c.wantNum, c.wantTit, c.wantOk)
		}
	}
}

func TestContiguous(t *testing.T) {
	ch := func(nums ...int) []Chapter {
		out := make([]Chapter, len(nums))
		for i, n := range nums {
			out[i] = Chapter{Chapter: n, Start: float64(i)}
		}
		return out
	}
	cases := []struct {
		name string
		chs  []Chapter
		want bool
	}{
		{"1..3", ch(1, 2, 3), true},
		{"0..2 front matter", ch(0, 1, 2), true},
		{"gap", ch(1, 2, 4), false},
		{"starts at 2", ch(2, 3, 4), false},
		{"empty", ch(), false},
		{"single 1", ch(1), true},
		{"duplicate", ch(1, 1, 2), false},
	}
	for _, c := range cases {
		if got := contiguous(c.chs); got != c.want {
			t.Errorf("%s: contiguous = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		Source: "/x/book.m4b", Title: "Book", Style: StyleMarkers, Duration: 30,
		ChapterCount: 2,
		Chapters: []Chapter{
			{Chapter: 1, Title: "A", MarkerTitle: "Chapter 1: A", Start: 0, End: 15, Duration: 15},
			{Chapter: 2, Title: "B", MarkerTitle: "Chapter 2: B", Start: 15, End: 30, Duration: 15},
		},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ChapterCount != 2 || got.Chapters[1].Title != "B" || got.Style != StyleMarkers {
		t.Errorf("round-trip manifest = %+v", got)
	}
}

func TestCompleteNearZeroDuration(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "tiny.flac")
	if err := os.WriteFile(small, make([]byte, 40), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	// A sub-minFlacBytes file for a normal-length chapter is NOT complete (a real
	// truncation), so resume re-splits it.
	if complete(small, 30.0) {
		t.Error("a sub-minFlacBytes file for a 30s chapter should not be complete")
	}
	// The same tiny file for a sub-second chapter IS complete, so resume does not
	// re-split a legitimately near-silent short chapter forever.
	if !complete(small, 0.3) {
		t.Error("a sub-second chapter with a non-empty file should be complete")
	}
	// A zero-byte file is never complete, even for a short chapter.
	empty := filepath.Join(dir, "empty.flac")
	if err := os.WriteFile(empty, nil, 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if complete(empty, 0.3) {
		t.Error("an empty file must not count as complete")
	}
}

// TestNaturalOrder is this repo's CONSUMER-LEVEL regression for the multi-file
// ordering contract: the comparator now lives upstream (audiosilo-meta
// pkg/scan.NaturalLess - one shared implementation, no local copy to drift), and
// this test guards both the import wiring and the ordering this package depends
// on, since the split order determines chapter numbers that spoiler-gate
// community sidecars (position.chapter). Upstream's natsort_test owns the
// exhaustive comparator cases.
func TestNaturalOrder(t *testing.T) {
	order := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.SliceStable(out, func(i, j int) bool { return scan.NaturalLess(out[i], out[j]) })
		return out
	}
	cases := []struct {
		name     string
		in, want []string
	}{
		{"unpadded", []string{"ch10", "ch1", "ch2"}, []string{"ch1", "ch2", "ch10"}},
		{"mixed padding", []string{"Chapter 10.mp3", "Chapter 1.mp3", "Chapter 02.mp3"},
			[]string{"Chapter 1.mp3", "Chapter 02.mp3", "Chapter 10.mp3"}},
		{"case-insensitive words", []string{"Beta 2", "alpha 10", "alpha 2"},
			[]string{"alpha 2", "alpha 10", "Beta 2"}},
	}
	for _, c := range cases {
		if got := order(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: natural order = %v, want %v", c.name, got, c.want)
		}
	}
	// Ties are stable: equal keys keep their input order under SliceStable.
	dup := order([]string{"track 3", "track 3", "track 1"})
	if !reflect.DeepEqual(dup, []string{"track 1", "track 3", "track 3"}) {
		t.Errorf("stable tie order = %v", dup)
	}

	// And the real consumer path: audioFilesIn returns a folder's audio files in
	// that same order ("Chapter 2" before "Chapter 10", non-audio ignored).
	dir := t.TempDir()
	for _, name := range []string{"Chapter 10.mp3", "Chapter 2.mp3", "Chapter 1.mp3", "cover.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
	files, err := audioFilesIn(dir)
	if err != nil {
		t.Fatalf("audioFilesIn: %v", err)
	}
	var bases []string
	for _, f := range files {
		bases = append(bases, filepath.Base(f))
	}
	want := []string{"Chapter 1.mp3", "Chapter 2.mp3", "Chapter 10.mp3"}
	if !reflect.DeepEqual(bases, want) {
		t.Errorf("audioFilesIn order = %v, want %v", bases, want)
	}
}

// --- ffmpeg-gated inspect + split ---

func requireFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed; skipping audio integration test")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed; skipping audio integration test")
	}
	return ffmpeg, ffprobe
}

// genChapteredM4B builds a tiny AAC .m4b with one chapter marker per title, each
// secs long, and returns its path. It skips the test if ffmpeg is unavailable.
func genChapteredM4B(t *testing.T, ffmpeg, dir string, titles []string, secs float64) string {
	t.Helper()
	total := secs * float64(len(titles))
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Fixture Book\n")
	for i, title := range titles {
		start := int(float64(i) * secs * 1000)
		end := int(float64(i+1) * secs * 1000)
		meta.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		meta.WriteString("START=" + strconv.Itoa(start) + "\n")
		meta.WriteString("END=" + strconv.Itoa(end) + "\n")
		meta.WriteString("title=" + title + "\n")
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	out := filepath.Join(dir, "book.m4b")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration="+ftoa(total),
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1",
		"-c:a", "aac", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate fixture m4b: %v", err)
	}
	return out
}

func TestInspectAndSplitMarkers(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// Exercise mixed marker styles in one book (all contiguous 1..3).
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1: One", "Chapter 2. Two", "Chapter 3 - Three"}, 3)
	work := filepath.Join(dir, "work")

	m, contig, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !contig {
		t.Fatal("markers should be contiguous")
	}
	if m.Style != StyleMarkers || m.ChapterCount != 3 {
		t.Fatalf("manifest = %+v, want 3 markers-style chapters", m)
	}
	if m.Chapters[2].Title != "Three" {
		t.Errorf("chapter 3 title = %q, want Three", m.Chapters[2].Title)
	}
	// probe.json + manifest.json written.
	for _, f := range []string{ProbeName, ManifestName} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Errorf("expected %s written: %v", f, err)
		}
	}

	// Split, then verify each FLAC is mono / 16 kHz / flac.
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, ch := range m.Chapters {
		p := filepath.Join(work, ChaptersDir, ChapterFileName(ch.Chapter))
		codec, chans, rate := probeFlac(t, ffprobe, p)
		if codec != "flac" || chans != 1 || rate != 16000 {
			t.Errorf("chapter %d FLAC = codec %q, %d ch, %d Hz; want flac/1/16000", ch.Chapter, codec, chans, rate)
		}
	}
}

func TestSplitResumesAfterDeletingOne(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1", "Chapter 2", "Chapter 3"}, 2)
	work := filepath.Join(dir, "work")
	m, _, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split #1: %v", err)
	}

	// Record mtimes of ch001/ch003, delete ch002, re-split, and confirm the kept
	// FLACs were NOT rewritten (same mtime) while the deleted one is restored.
	ch1 := filepath.Join(work, ChaptersDir, ChapterFileName(1))
	ch2 := filepath.Join(work, ChaptersDir, ChapterFileName(2))
	ch3 := filepath.Join(work, ChaptersDir, ChapterFileName(3))
	mt1, mt3 := mtime(t, ch1), mtime(t, ch3)
	if err := os.Remove(ch2); err != nil {
		t.Fatal(err)
	}
	// Progress must still report every chapter (skipped + redone) up to total.
	var last int
	if err := Split(context.Background(), m, work, ffmpeg, func(done, total int) {
		if total != 3 {
			t.Errorf("progress total = %d, want 3", total)
		}
		last = done
	}); err != nil {
		t.Fatalf("Split #2: %v", err)
	}
	if last != 3 {
		t.Errorf("final progress done = %d, want 3", last)
	}
	if mtime(t, ch1) != mt1 || mtime(t, ch3) != mt3 {
		t.Error("kept chapters were rewritten on resume (mtime changed)")
	}
	if !complete(ch2, m.Chapters[1].Duration) {
		t.Error("deleted chapter was not restored on resume")
	}
}

func TestInspectMultiFileStyle(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "multi")
	if err := os.MkdirAll(bookDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Two loose single-chapter files -> a files-style book.
	for _, name := range []string{"01 - Part A.mp3", "02 - Part B.mp3"} {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency=200:duration=2",
			"-c:a", "libmp3lame", filepath.Join(bookDir, name))
		if err := cmd.Run(); err != nil {
			t.Skipf("mp3 encoder unavailable: %v", err)
		}
	}
	work := filepath.Join(dir, "work")
	m, contig, err := Inspect(context.Background(), bookDir, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !contig || m.Style != StyleFiles || m.ChapterCount != 2 {
		t.Fatalf("multi-file manifest = %+v, want 2 files-style chapters", m)
	}
	if m.Chapters[0].FilePath == "" || m.Chapters[1].Chapter != 2 {
		t.Errorf("files-style chapters = %+v", m.Chapters)
	}
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, ch := range m.Chapters {
		p := filepath.Join(work, ChaptersDir, ChapterFileName(ch.Chapter))
		codec, chans, rate := probeFlac(t, ffprobe, p)
		if codec != "flac" || chans != 1 || rate != 16000 {
			t.Errorf("chapter %d FLAC = %q/%d/%d, want flac/1/16000", ch.Chapter, codec, chans, rate)
		}
	}
}

func TestInspectNonContiguousWritesDraftManifest(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// A gap (1,2,4) is non-contiguous -> contiguous=false, but a DRAFT manifest is
	// written for the markers_normalizing agent stage to correct.
	book := genChapteredM4B(t, ffmpeg, dir,
		[]string{"Chapter 1", "Chapter 2", "Chapter 4"}, 2)
	work := filepath.Join(dir, "work")
	m, contig, err := Inspect(context.Background(), book, work, ffprobe)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if contig {
		t.Error("gap markers should not be contiguous")
	}
	// The draft manifest is written (non-contiguous chapters preserved as-seen).
	if _, err := os.Stat(filepath.Join(work, ManifestName)); err != nil {
		t.Errorf("non-contiguous inspect should write a draft manifest: %v", err)
	}
	if len(m.Chapters) != 3 || Contiguous(m.Chapters) {
		t.Errorf("draft manifest should carry the 3 non-contiguous chapters, got %+v", m.Chapters)
	}
	// probe.json is still written (the record of what we saw).
	if _, err := os.Stat(filepath.Join(work, ProbeName)); err != nil {
		t.Errorf("probe.json should be written even when non-contiguous: %v", err)
	}
}

// probeFlac returns a FLAC's codec, channel count, and sample rate via ffprobe.
func probeFlac(t *testing.T, ffprobe, path string) (codec string, channels, sampleRate int) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,channels,sample_rate",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe flac %s: %v", path, err)
	}
	var parsed struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		t.Fatalf("parse ffprobe flac json: %v (%s)", err, out)
	}
	rate, _ := strconv.Atoi(parsed.Streams[0].SampleRate)
	return parsed.Streams[0].CodecName, parsed.Streams[0].Channels, rate
}

func mtime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UnixNano()
}
