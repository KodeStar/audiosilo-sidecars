package transcript

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeNonFinite(t *testing.T) {
	// NaN / Infinity / -Infinity outside strings become null; the result parses.
	raw := []byte(`{"a": NaN, "b": Infinity, "c": -Infinity, "d": 1.5}`)
	got := Sanitize(raw)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("sanitized output does not parse: %v (%s)", err, got)
	}
	for _, k := range []string{"a", "b", "c"} {
		if m[k] != nil {
			t.Errorf("%s = %v, want null", k, m[k])
		}
	}
	if m["d"] != 1.5 {
		t.Errorf("d = %v, want 1.5", m["d"])
	}
}

func TestSanitizePreservesStrings(t *testing.T) {
	// A literal that appears INSIDE transcript text must be left untouched, and
	// escaped quotes must not confuse the string tracking.
	raw := []byte(`{"text": "he said \"NaN\" and Infinity too", "v": NaN}`)
	got := Sanitize(raw)
	var d struct {
		Text string   `json:"text"`
		V    *float64 `json:"v"`
	}
	if err := json.Unmarshal(got, &d); err != nil {
		t.Fatalf("parse: %v (%s)", err, got)
	}
	if d.Text != `he said "NaN" and Infinity too` {
		t.Errorf("string text mangled: %q", d.Text)
	}
	if d.V != nil {
		t.Errorf("out-of-string NaN not nulled: %v", d.V)
	}
}

func TestSanitizeNoChange(t *testing.T) {
	raw := []byte(`{"a":1,"b":"plain"}`)
	if got := Sanitize(raw); string(got) != string(raw) {
		t.Errorf("clean JSON was altered: %s", got)
	}
}

func TestNormalizeOpenAIFixture(t *testing.T) {
	raw := readFixture(t, "mlx-ch001.raw.json")
	if !Complete(raw) {
		t.Fatal("fixture should be Complete")
	}
	tr, err := Normalize(raw, Meta{Chapter: 1, Backend: "mlx-whisper", Model: "large-v3-turbo", Language: "en"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if tr.Schema != Schema || tr.Chapter != 1 || tr.Backend != "mlx-whisper" {
		t.Errorf("provenance wrong: %+v", tr)
	}
	if tr.Language != "en" {
		t.Errorf("language = %q, want en", tr.Language)
	}
	if len(tr.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(tr.Segments))
	}
	if tr.Segments[0].Words[0].W != " Chapter" {
		t.Errorf("first word = %q", tr.Segments[0].Words[0].W)
	}
	if p := tr.Segments[0].Words[0].P; p == nil || *p != 0.98 {
		t.Errorf("first word probability = %v, want 0.98", p)
	}
	// The final word had probability NaN -> nil after normalization.
	last := tr.Segments[1].Words
	if last[len(last)-1].P != nil {
		t.Errorf("NaN word probability should normalize to nil")
	}
	// The whole normalized doc marshals cleanly (no non-finite leaked through).
	if _, err := json.Marshal(tr); err != nil {
		t.Fatalf("normalized transcript does not marshal: %v", err)
	}
	if txt := PlainText(tr); txt != "Chapter one. The road went ever on." {
		t.Errorf("plain text = %q", txt)
	}
}

func TestAdaptOpenAIPreservesSegmentIDs(t *testing.T) {
	// Both segments report id 0 - the second at loop index 1. The reported id must
	// pass through verbatim; the old loop-index fabrication would have rewritten the
	// second segment's id to 1, corrupting a genuinely id-0 segment.
	raw := []byte(`{
		"text": "one two",
		"language": "en",
		"segments": [
			{"id": 0, "start": 0.0, "end": 1.0, "text": " one", "words": []},
			{"id": 0, "start": 1.0, "end": 2.0, "text": " two", "words": []}
		]
	}`)
	tr, err := Normalize(raw, Meta{Chapter: 1, Backend: "mlx-whisper", Model: "large-v3-turbo", Language: "en"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(tr.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(tr.Segments))
	}
	if tr.Segments[0].ID != 0 {
		t.Errorf("segment 0 id = %d, want 0", tr.Segments[0].ID)
	}
	if tr.Segments[1].ID != 0 {
		t.Errorf("segment 1 id = %d, want 0 (loop-index fabrication should be gone)", tr.Segments[1].ID)
	}
}

func TestNormalizeWhisperCppFixture(t *testing.T) {
	raw := readFixture(t, "whispercpp-ch001.raw.json")
	if !Complete(raw) {
		t.Fatal("fixture should be Complete")
	}
	tr, err := Normalize(raw, Meta{Chapter: 1, Backend: "whisper-cpp", Model: "ggml-large-v3-turbo", Language: "en"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if tr.Backend != "whisper-cpp" || tr.Language != "en" {
		t.Errorf("provenance wrong: %+v", tr)
	}
	if len(tr.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(tr.Segments))
	}
	// The [_BEG_] control token is dropped; the first real word is " Chapter".
	w0 := tr.Segments[0].Words
	if len(w0) != 2 || w0[0].W != " Chapter" {
		t.Errorf("segment 0 words = %+v, want [Chapter one.] without control token", w0)
	}
	// Token offsets are milliseconds -> seconds.
	if w0[0].Start != 0.0 || w0[0].End != 0.6 {
		t.Errorf("word timing = %v-%v, want 0-0.6s", w0[0].Start, w0[0].End)
	}
	if tr.Segments[1].End != 5.0 {
		t.Errorf("segment 1 end = %v, want 5.0s", tr.Segments[1].End)
	}
	if txt := PlainText(tr); txt != "Chapter one. The road went ever on." {
		t.Errorf("plain text = %q", txt)
	}
}

func TestCompleteRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"truncated":      []byte(`{"text": " partial", "segm`),
		"empty":          []byte(``),
		"not json":       []byte(`not json at all`),
		"no arrays":      []byte(`{"text": "hi"}`),
		"openai no text": []byte(`{"segments": []}`),
	}
	for name, raw := range cases {
		if Complete(raw) {
			t.Errorf("%s: Complete = true, want false", name)
		}
	}
	// A minimal-but-structurally-complete openai doc passes.
	if !Complete([]byte(`{"text": "", "segments": []}`)) {
		t.Error("minimal complete openai doc should pass")
	}
	// A whisper.cpp doc with an empty transcription array passes structurally.
	if !Complete([]byte(`{"transcription": []}`)) {
		t.Error("minimal complete whisper.cpp doc should pass")
	}
}

func TestWriteNormalizedAndText(t *testing.T) {
	dir := t.TempDir()
	tr := Transcript{
		Schema: Schema, Chapter: 3, Backend: "mlx-whisper", Language: "en",
		Segments: []Segment{{ID: 0, Start: 0, End: 1, Text: " Hello", Words: []Word{{W: " Hello", Start: 0, End: 1}}}},
	}
	jsonDir := filepath.Join(dir, JSONDir)
	textDir := filepath.Join(dir, TextDir)
	if err := WriteNormalized(jsonDir, tr); err != nil {
		t.Fatalf("WriteNormalized: %v", err)
	}
	if err := WriteText(textDir, tr.Chapter, PlainText(tr)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	// The JSON round-trips and lands at chNNN.json.
	rawOut, err := os.ReadFile(filepath.Join(jsonDir, "ch003.json"))
	if err != nil {
		t.Fatalf("read normalized: %v", err)
	}
	var back Transcript
	if err := json.Unmarshal(rawOut, &back); err != nil {
		t.Fatalf("normalized json invalid: %v", err)
	}
	if back.Chapter != 3 || back.Schema != Schema {
		t.Errorf("round-trip = %+v", back)
	}
	txt, err := os.ReadFile(filepath.Join(textDir, "ch003.txt"))
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	if string(txt) != "Hello\n" {
		t.Errorf("text = %q, want \"Hello\\n\"", txt)
	}
}

func TestReadNormalizedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jsonDir := filepath.Join(dir, JSONDir)
	tr := Transcript{
		Schema: Schema, Chapter: 7, Backend: "mlx-whisper", Model: "large-v3", Language: "en",
		Segments: []Segment{{ID: 0, Start: 0, End: 1, Text: " Hi", Words: []Word{{W: " Hi", Start: 0, End: 1}}}},
	}
	if err := WriteNormalized(jsonDir, tr); err != nil {
		t.Fatalf("WriteNormalized: %v", err)
	}
	got, err := ReadNormalized(jsonDir, 7)
	if err != nil {
		t.Fatalf("ReadNormalized: %v", err)
	}
	if got.Chapter != 7 || got.Model != "large-v3" || len(got.Segments) != 1 || got.Segments[0].Text != " Hi" {
		t.Errorf("round-trip = %+v", got)
	}
}

func TestReadNormalizedRejectsWrongSchema(t *testing.T) {
	dir := t.TempDir()
	// A document with a foreign schema must be refused, not read as v1.
	if err := os.WriteFile(filepath.Join(dir, JSONName(2)), []byte(`{"schema":"other/v9","chapter":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNormalized(dir, 2); err == nil {
		t.Fatal("ReadNormalized should reject a non-v1 schema")
	}
	// A missing file surfaces its read error.
	if _, err := ReadNormalized(dir, 99); err == nil {
		t.Fatal("ReadNormalized should error on a missing file")
	}
}

func TestFiniteHelper(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	ok := 0.5
	if finite(&nan) != nil || finite(&inf) != nil || finite(nil) != nil {
		t.Error("non-finite/nil should map to nil")
	}
	if got := finite(&ok); got == nil || *got != 0.5 {
		t.Errorf("finite(0.5) = %v", got)
	}
}

func TestWritersRejectRawLayer(t *testing.T) {
	root := t.TempDir()
	rawDir := filepath.Join(root, RawDir)

	if err := WriteNormalized(rawDir, Transcript{Schema: Schema, Chapter: 1}); err == nil {
		t.Error("WriteNormalized into transcripts-raw should be refused")
	}
	if err := WriteText(rawDir, 1, "text"); err == nil {
		t.Error("WriteText into transcripts-raw should be refused")
	}
	// Nothing was written into the guarded dir.
	if _, err := os.Stat(rawDir); err == nil {
		t.Error("refused write must not create the raw dir")
	}

	// A normal derived dir is accepted.
	jsonDir := filepath.Join(root, JSONDir)
	if err := WriteNormalized(jsonDir, Transcript{Schema: Schema, Chapter: 1}); err != nil {
		t.Errorf("WriteNormalized into %s: %v", JSONDir, err)
	}
	if err := WriteText(filepath.Join(root, TextDir), 1, "text"); err != nil {
		t.Errorf("WriteText into %s: %v", TextDir, err)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestChapterTextPath(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, TextDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, RepairedDir), 0o750); err != nil {
		t.Fatal(err)
	}

	// Neither layer present -> not found.
	if p, ok := ChapterTextPath(work, 1); ok {
		t.Errorf("ChapterTextPath(no layers) = (%q, true), want not found", p)
	}

	// Base text only -> the text path.
	textPath := filepath.Join(work, TextDir, TextName(1))
	if err := os.WriteFile(textPath, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, ok := ChapterTextPath(work, 1); !ok || p != textPath {
		t.Errorf("ChapterTextPath(text only) = (%q, %v), want (%q, true)", p, ok, textPath)
	}

	// Repaired present -> prefer it over the base text.
	repPath := filepath.Join(work, RepairedDir, TextName(1))
	if err := os.WriteFile(repPath, []byte("repaired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, ok := ChapterTextPath(work, 1); !ok || p != repPath {
		t.Errorf("ChapterTextPath(both) = (%q, %v), want (%q, true) [repaired preferred]", p, ok, repPath)
	}
}
