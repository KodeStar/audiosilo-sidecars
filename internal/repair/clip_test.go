package repair

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// rawOpenAI builds a minimal openai-whisper raw JSON with the given plain text as its
// single segment, which transcript.Normalize accepts.
func rawOpenAI(text string) []byte {
	doc := map[string]any{
		"text":     text,
		"language": "en",
		"segments": []map[string]any{
			{"id": 0, "start": 0.0, "end": 1.0, "text": text, "words": []any{}},
		},
	}
	b, _ := json.Marshal(doc)
	return b
}

// fakeCut writes a dummy clip file so ClipAndSplice's transcribe step proceeds; it
// never touches ffmpeg.
func fakeCut(_ context.Context, _, dstFlac string, _, _ float64) error {
	return os.WriteFile(dstFlac, []byte("flac"), 0o644)
}

func fakeTranscribe(text string) TranscribeClip {
	return func(_ context.Context, _ string) ([]byte, error) {
		return rawOpenAI(text), nil
	}
}

func locatedTranscript() transcript.Transcript {
	phrase := []string{"and", "then", "he", "ran", "away", "fast"}
	toks := loopTokens(60, phrase, 12, 100.0, 0.05)
	return buildTranscript(4, 8, toks)
}

func TestClipAndSplice_FabricatedSplices(t *testing.T) {
	dir := t.TempDir()
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    4,
		Transcript: locatedTranscript(),
		ChapterEnd: 200.0,
		Cut:        fakeCut,
		Transcribe: fakeTranscribe("he walked to the door and left the room quietly"),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if !res.Located {
		t.Fatal("expected Located=true")
	}
	if res.Verdict != VerdictFabricated {
		t.Errorf("verdict = %s, want FABRICATED", res.Verdict)
	}
	if !res.Spliced {
		t.Fatal("expected Spliced=true")
	}
	// transcripts-repaired/ch004.txt written and does not contain the loop.
	repaired := filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4))
	body, err := os.ReadFile(repaired)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if strings.Contains(string(body), "and then he ran away fast and then") {
		t.Error("repaired text still contains the loop")
	}
	if !strings.Contains(string(body), "walked to the door") {
		t.Error("repaired text missing the fresh clip ending")
	}
	// repairs.log has the entry.
	log := readFile(t, filepath.Join(dir, RepairsLogName))
	if !strings.Contains(log, "- ch004 [FABRICATED]") {
		t.Errorf("repairs.log missing entry:\n%s", log)
	}
	// tail_verdicts.json has the chapter.
	vs, err := LoadTailVerdicts(dir)
	if err != nil {
		t.Fatalf("LoadTailVerdicts: %v", err)
	}
	if len(vs) != 1 || vs[0].Chapter != 4 || vs[0].Verdict != VerdictFabricated {
		t.Errorf("tail_verdicts = %+v", vs)
	}
}

func TestClipAndSplice_RedegeneratedKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    4,
		Transcript: locatedTranscript(),
		ChapterEnd: 200.0,
		Cut:        fakeCut,
		// The fresh clip loops too (unhealthy): keep the original, no splice.
		Transcribe: fakeTranscribe(strings.Repeat("and then he ran away fast ", 5)),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.ClipHealthy {
		t.Error("expected the looping clip to be unhealthy")
	}
	if res.Verdict != VerdictClipRedegenerated {
		t.Errorf("verdict = %s, want CLIP-REDEGENERATED", res.Verdict)
	}
	if res.Spliced {
		t.Error("expected no splice on a re-degenerated clip")
	}
	if _, err := os.Stat(filepath.Join(dir, transcript.RepairedDir, transcript.TextName(4))); !os.IsNotExist(err) {
		t.Error("expected no repaired file")
	}
	if _, err := os.Stat(filepath.Join(dir, RepairsLogName)); !os.IsNotExist(err) {
		t.Error("expected no repairs.log entry")
	}
	// The verdict is still recorded.
	vs, _ := LoadTailVerdicts(dir)
	if len(vs) != 1 || vs[0].Verdict != VerdictClipRedegenerated {
		t.Errorf("tail_verdicts = %+v", vs)
	}
}

func TestClipAndSplice_NotLocatedNoOp(t *testing.T) {
	dir := t.TempDir()
	// A distinct-word transcript with no repeated 6-gram: nothing to locate.
	tr := buildTranscript(7, 8, loopTokens(60, nil, 0, 0, 0))
	req := ClipSpliceRequest{
		WorkDir:    dir,
		Chapter:    7,
		Transcript: tr,
		ChapterEnd: 30.0,
		Cut:        fakeCut,
		Transcribe: fakeTranscribe("unused"),
	}
	res, err := ClipAndSplice(context.Background(), req)
	if err != nil {
		t.Fatalf("ClipAndSplice: %v", err)
	}
	if res.Located || res.Spliced {
		t.Errorf("expected no-op, got %+v", res)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected no artifacts written, got %d entries", len(entries))
	}
}

// --- AppendRepairLog / MergeTailVerdict -------------------------------------

func TestAppendRepairLog_HeaderThenAppend(t *testing.T) {
	dir := t.TempDir()
	if err := AppendRepairLog(dir, "- ch001 [BENIGN]: x"); err != nil {
		t.Fatal(err)
	}
	if err := AppendRepairLog(dir, "- ch002 [FABRICATED]: y"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, RepairsLogName))
	want := repairsLogHeader + "\n\n- ch001 [BENIGN]: x\n- ch002 [FABRICATED]: y\n"
	if got != want {
		t.Errorf("repairs.log =\n%q\nwant\n%q", got, want)
	}
}

func TestMergeTailVerdict_UpsertsAndSorts(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 5, Verdict: VerdictBenign, InClip: 1}))
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 2, Verdict: VerdictFabricated}))
	// Re-entry replaces ch5's verdict.
	must(MergeTailVerdict(dir, TailVerdict{Chapter: 5, Verdict: VerdictClipRedegenerated, InClip: 4}))

	vs, err := LoadTailVerdicts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(vs), vs)
	}
	if vs[0].Chapter != 2 || vs[1].Chapter != 5 {
		t.Errorf("not sorted by chapter: %+v", vs)
	}
	if vs[1].Verdict != VerdictClipRedegenerated || vs[1].InClip != 4 {
		t.Errorf("ch5 not upserted: %+v", vs[1])
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
