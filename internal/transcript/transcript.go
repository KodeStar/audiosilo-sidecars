// Package transcript defines the normalized ASR transcript contract
// (audiosilo-transcript/v1) and owns everything downstream of a raw backend
// output: sanitizing the raw JSON (non-finite numbers -> null), adapting each
// backend's native shape into the one normalized form, and writing the derived
// transcripts-json/ and transcripts-text/ artifacts. The raw backend output is
// preserved byte-for-byte and immutable in transcripts-raw/; this package never
// writes there.
//
// The work-dir layout mirrors the historical EXTRACTION-AUDIO.md conventions so
// past work dirs stay readable:
//
//	transcripts-raw/chNNN.json   raw backend output (immutable, 0444)
//	transcripts-json/chNNN.json  normalized audiosilo-transcript/v1 (this package)
//	transcripts-text/chNNN.txt   concatenated segment text (this package)
//
// Two raw formats are recognized and auto-detected: openai-whisper / mlx-whisper
// (top-level "segments"+"text", per-word "word"/"probability", and MLX's
// non-finite "avg_logprob" that strict JSON rejects) and whisper.cpp -ojf
// (top-level "transcription", per-token "offsets"/"p"). Both collapse to the same
// segments+words shape so later stages are backend-agnostic.
package transcript

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Schema is the version tag every normalized transcript carries.
const Schema = "audiosilo-transcript/v1"

// Work-dir subdirectories for the three transcript layers.
const (
	RawDir  = "transcripts-raw"
	JSONDir = "transcripts-json"
	TextDir = "transcripts-text"
)

// Word is one word (openai/mlx) or token (whisper.cpp) with its timing. P is the
// model's probability for the word, or nil when the backend did not report it or
// reported a non-finite value.
type Word struct {
	W     string   `json:"w"`
	Start float64  `json:"start"`
	End   float64  `json:"end"`
	P     *float64 `json:"p"`
}

// Segment is one transcript segment: a text span with its timing and, when the
// backend emitted word timestamps, the words within it.
type Segment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	Words []Word  `json:"words"`
}

// Transcript is the normalized audiosilo-transcript/v1 document. Backend/Model/
// Language are provenance carried from the ASR stage (see Meta).
type Transcript struct {
	Schema   string    `json:"schema"`
	Chapter  int       `json:"chapter"`
	Backend  string    `json:"backend"`
	Model    string    `json:"model"`
	Language string    `json:"language"`
	Segments []Segment `json:"segments"`
}

// Meta is the provenance the ASR stage supplies when normalizing a raw file. The
// raw output does not reliably carry the backend/model that produced it, so the
// caller passes them through; Language is a fallback used only when the raw
// document does not state its own.
type Meta struct {
	Chapter  int
	Backend  string
	Model    string
	Language string
}

// format classifies a raw ASR document.
type format int

const (
	formatUnknown format = iota
	formatOpenAI         // openai-whisper / mlx-whisper
	formatWhisperCpp
)

// Name is the shared chapter stem ("ch%03d"), matching the FLAC/raw naming so the
// three transcript layers line up per chapter.
func Name(chapter int) string { return fmt.Sprintf("ch%03d", chapter) }

// JSONName is the normalized-transcript filename for a chapter.
func JSONName(chapter int) string { return Name(chapter) + ".json" }

// TextName is the plain-text filename for a chapter.
func TextName(chapter int) string { return Name(chapter) + ".txt" }

// RawName is the raw-output filename a backend writes for a chapter (a backend's
// own output naming derives from the input FLAC stem, which is this same stem).
func RawName(chapter int) string { return Name(chapter) + ".json" }

// Complete reports whether raw is a structurally complete transcript for either
// recognized format: a parseable document whose primary array (segments /
// transcription) is present. It ports audio_extract.py's transcript_is_complete
// (adding whisper.cpp) and is the resume/skip test - an interrupted or truncated
// output fails it and is re-transcribed. It sanitizes first, since MLX emits
// non-finite numbers that a strict JSON parse would reject.
func Complete(raw []byte) bool {
	s := Sanitize(raw)
	switch detect(s) {
	case formatOpenAI:
		var d struct {
			Text     *string            `json:"text"`
			Segments *[]json.RawMessage `json:"segments"`
		}
		if json.Unmarshal(s, &d) != nil {
			return false
		}
		return d.Text != nil && d.Segments != nil
	case formatWhisperCpp:
		var d struct {
			Transcription *[]json.RawMessage `json:"transcription"`
		}
		if json.Unmarshal(s, &d) != nil {
			return false
		}
		return d.Transcription != nil
	default:
		return false
	}
}

// Normalize sanitizes raw and adapts it (per detected format) into an
// audiosilo-transcript/v1 document, stamping the provenance from meta. Language
// prefers the raw document's own value, falling back to meta.Language.
func Normalize(raw []byte, meta Meta) (Transcript, error) {
	s := Sanitize(raw)
	switch detect(s) {
	case formatOpenAI:
		return adaptOpenAI(s, meta)
	case formatWhisperCpp:
		return adaptWhisperCpp(s, meta)
	default:
		return Transcript{}, fmt.Errorf("unrecognized transcript format")
	}
}

// detect classifies sanitized JSON by which primary array key is present. A
// whisper.cpp document has "transcription"; an openai/mlx one has "segments".
func detect(sanitized []byte) format {
	var probe struct {
		Segments      json.RawMessage `json:"segments"`
		Transcription json.RawMessage `json:"transcription"`
	}
	if json.Unmarshal(sanitized, &probe) != nil {
		return formatUnknown
	}
	switch {
	case len(probe.Transcription) > 0:
		return formatWhisperCpp
	case len(probe.Segments) > 0:
		return formatOpenAI
	default:
		return formatUnknown
	}
}

// --- openai-whisper / mlx-whisper adapter ---

type owWord struct {
	Word        string   `json:"word"`
	Start       float64  `json:"start"`
	End         float64  `json:"end"`
	Probability *float64 `json:"probability"`
}

type owSegment struct {
	ID    int      `json:"id"`
	Start float64  `json:"start"`
	End   float64  `json:"end"`
	Text  string   `json:"text"`
	Words []owWord `json:"words"`
}

type owDoc struct {
	Text     string      `json:"text"`
	Language string      `json:"language"`
	Segments []owSegment `json:"segments"`
}

func adaptOpenAI(sanitized []byte, meta Meta) (Transcript, error) {
	var d owDoc
	if err := json.Unmarshal(sanitized, &d); err != nil {
		return Transcript{}, fmt.Errorf("parse openai-whisper transcript: %w", err)
	}
	segs := make([]Segment, 0, len(d.Segments))
	for i, s := range d.Segments {
		words := make([]Word, 0, len(s.Words))
		for _, w := range s.Words {
			words = append(words, Word{W: w.Word, Start: w.Start, End: w.End, P: finite(w.Probability)})
		}
		id := s.ID
		if id == 0 && i != 0 {
			id = i
		}
		segs = append(segs, Segment{ID: id, Start: s.Start, End: s.End, Text: s.Text, Words: words})
	}
	return Transcript{
		Schema:   Schema,
		Chapter:  meta.Chapter,
		Backend:  meta.Backend,
		Model:    meta.Model,
		Language: firstNonEmpty(d.Language, meta.Language),
		Segments: segs,
	}, nil
}

// --- whisper.cpp -ojf adapter ---

type wcOffsets struct {
	From int `json:"from"` // milliseconds
	To   int `json:"to"`
}

type wcToken struct {
	Text    string    `json:"text"`
	Offsets wcOffsets `json:"offsets"`
	P       *float64  `json:"p"`
}

type wcSegment struct {
	Offsets wcOffsets `json:"offsets"`
	Text    string    `json:"text"`
	Tokens  []wcToken `json:"tokens"`
}

type wcDoc struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Params struct {
		Language string `json:"language"`
	} `json:"params"`
	Transcription []wcSegment `json:"transcription"`
}

func adaptWhisperCpp(sanitized []byte, meta Meta) (Transcript, error) {
	var d wcDoc
	if err := json.Unmarshal(sanitized, &d); err != nil {
		return Transcript{}, fmt.Errorf("parse whisper.cpp transcript: %w", err)
	}
	segs := make([]Segment, 0, len(d.Transcription))
	for i, s := range d.Transcription {
		words := make([]Word, 0, len(s.Tokens))
		for _, tk := range s.Tokens {
			if isSpecialToken(tk.Text) {
				continue // whisper.cpp emits [_BEG_]/timestamp control tokens; skip them
			}
			words = append(words, Word{
				W:     tk.Text,
				Start: msToSec(tk.Offsets.From),
				End:   msToSec(tk.Offsets.To),
				P:     finite(tk.P),
			})
		}
		segs = append(segs, Segment{
			ID:    i,
			Start: msToSec(s.Offsets.From),
			End:   msToSec(s.Offsets.To),
			Text:  s.Text,
			Words: words,
		})
	}
	return Transcript{
		Schema:   Schema,
		Chapter:  meta.Chapter,
		Backend:  meta.Backend,
		Model:    meta.Model,
		Language: firstNonEmpty(d.Result.Language, d.Params.Language, meta.Language),
		Segments: segs,
	}, nil
}

// isSpecialToken reports whether a whisper.cpp token is a control/special token
// (e.g. "[_BEG_]", "[_TT_123]") rather than spoken text.
func isSpecialToken(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[_")
}

func msToSec(ms int) float64 { return float64(ms) / 1000.0 }

// finite returns p unless it is nil or non-finite, in which case nil (so the
// normalized JSON carries null rather than a value a strict reader rejects).
func finite(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// PlainText returns the transcript's text as the concatenation of its segment
// texts (whisper segment texts include their own leading spaces), trimmed.
func PlainText(t Transcript) string {
	var b strings.Builder
	for _, s := range t.Segments {
		b.WriteString(s.Text)
	}
	return strings.TrimSpace(b.String())
}

// WriteNormalized writes t to jsonDir/chNNN.json (indented, trailing newline)
// atomically. jsonDir is created if absent.
func WriteNormalized(jsonDir string, t Transcript) error {
	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(jsonDir, JSONName(t.Chapter)), append(out, '\n'))
}

// WriteText writes the plain chapter text to textDir/chNNN.txt (trailing newline)
// atomically. textDir is created if absent.
func WriteText(textDir string, chapter int, text string) error {
	return writeFileAtomic(filepath.Join(textDir, TextName(chapter)), []byte(text+"\n"))
}

// writeFileAtomic writes data via a sibling temp file + rename so a crash never
// leaves a half-written artifact a later run would trust.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // non-secret artifact
		return err
	}
	return os.Rename(tmp, path)
}
