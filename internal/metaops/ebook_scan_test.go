package metaops

import (
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

func cand(dir, path, title, isbn string) ebook.Candidate {
	return ebook.Candidate{Path: path, Dir: dir, Title: title, ISBN: isbn}
}

// TestAnnotateEbookBesideAudiobook: when an epub sits in an audiobook's folder the
// epub drives the pipeline, but SOURCE_PATH STAYS THE FOLDER. That split is the
// point - source_path is the durable identity every override, enqueue and queue row
// keys on, and the audio tags carry the ASIN, the strongest coverage-match key.
func TestAnnotateEbookBesideAudiobook(t *testing.T) {
	sb := ScannedBook{SourcePath: "/lib/Some Book", Title: "Some Book", ASIN: "B00X"}
	byDir := ebook.ByDir(map[string]ebook.Candidate{
		"/lib/Some Book/b.epub": cand("/lib/Some Book", "/lib/Some Book/b.epub", "Some Book", "9780141439518"),
	})
	claimed := map[string]bool{}
	annotateEbook(&sb, byDir, claimed, nil)

	if sb.Kind != string(state.KindEbook) {
		t.Errorf("kind = %q, want ebook", sb.Kind)
	}
	if sb.SourcePath != "/lib/Some Book" {
		t.Errorf("source_path = %q; it must stay the audiobook folder", sb.SourcePath)
	}
	if sb.EbookPath != "/lib/Some Book/b.epub" {
		t.Errorf("ebook_path = %q", sb.EbookPath)
	}
	if sb.ASIN != "B00X" {
		t.Error("the audio ASIN was lost; tags outrank an epub's OPF")
	}
	// The epub filled only what the audio scan left empty.
	if sb.ISBN != "9780141439518" || sb.Sources["isbn"] != "epub" {
		t.Errorf("isbn = %q (source %q), want it filled from the epub with provenance", sb.ISBN, sb.Sources["isbn"])
	}
	if !claimed["/lib/Some Book/b.epub"] {
		t.Error("the epub was not claimed, so it will also appear as its own candidate")
	}
}

// TestAnnotateEbookNeverOverwritesTags: a calibre series field is frequently wrong
// (one corpus book self-reports the wrong series), so an epub may only fill gaps.
func TestAnnotateEbookNeverOverwritesTags(t *testing.T) {
	sb := ScannedBook{SourcePath: "/lib/B", ISBN: "9780000000002", Series: "Real Series"}
	byDir := ebook.ByDir(map[string]ebook.Candidate{
		"/lib/B/x.epub": {Path: "/lib/B/x.epub", Dir: "/lib/B", ISBN: "9780141439518", Series: "Wrong Series"},
	})
	annotateEbook(&sb, byDir, map[string]bool{}, nil)
	if sb.ISBN != "9780000000002" || sb.Series != "Real Series" {
		t.Errorf("the epub overwrote tag-derived identity: isbn=%q series=%q", sb.ISBN, sb.Series)
	}
}

// TestAnnotateEbookRefusesToGuessBetweenSeveral: picking one of several epubs would
// be a guess, and the wrong text attributes another book's plot to this one - the
// same hazard the extract stage quarantines cross-book excerpts for.
func TestAnnotateEbookRefusesToGuessBetweenSeveral(t *testing.T) {
	sb := ScannedBook{SourcePath: "/lib/C"}
	byDir := ebook.ByDir(map[string]ebook.Candidate{
		"/lib/C/one.epub": cand("/lib/C", "/lib/C/one.epub", "One", ""),
		"/lib/C/two.epub": cand("/lib/C", "/lib/C/two.epub", "Two", ""),
	})
	claimed := map[string]bool{}
	annotateEbook(&sb, byDir, claimed, nil)

	if sb.Kind != "" || sb.EbookPath != "" {
		t.Errorf("a folder with 2 epubs was resolved to one: kind=%q path=%q", sb.Kind, sb.EbookPath)
	}
	if sb.EbookNote == "" {
		t.Error("no note explaining why the epubs were ignored")
	}
	// Both are still claimed, so neither reappears as a phantom standalone book.
	if len(claimed) != 2 {
		t.Errorf("claimed = %v, want both epubs claimed", claimed)
	}
}

func TestAnnotateEbookHonoursForceAudio(t *testing.T) {
	sb := ScannedBook{SourcePath: "/lib/D"}
	byDir := ebook.ByDir(map[string]ebook.Candidate{
		"/lib/D/x.epub": cand("/lib/D", "/lib/D/x.epub", "X", ""),
	})
	ov := map[string]Override{"/lib/D": {ForceAudio: true}}
	annotateEbook(&sb, byDir, map[string]bool{}, ov)

	if sb.Kind == string(state.KindEbook) {
		t.Error("force_audio was ignored")
	}
	if sb.EbookPath == "" {
		t.Error("ebook_path should still be reported so the UI can say an epub is present")
	}
}

// TestAnnotateEbookUnreadableEpub: a DRM-wrapped epub must not silently become the
// text source, and the user should be told why their book is being transcribed.
func TestAnnotateEbookUnreadableEpub(t *testing.T) {
	sb := ScannedBook{SourcePath: "/lib/E"}
	byDir := ebook.ByDir(map[string]ebook.Candidate{
		"/lib/E/x.epub": {Path: "/lib/E/x.epub", Dir: "/lib/E", MetaErr: "not a zip"},
	})
	annotateEbook(&sb, byDir, map[string]bool{}, nil)
	if sb.Kind == string(state.KindEbook) {
		t.Error("an unreadable epub became the text source")
	}
	if sb.EbookNote == "" {
		t.Error("no note explaining the unreadable epub")
	}
}

// TestEbookOnlyCandidates: an unclaimed epub becomes a book in its own right, so a
// text-only library scans like an audio one.
func TestEbookOnlyCandidates(t *testing.T) {
	epubs := map[string]ebook.Candidate{
		"/lib/a.epub":       cand("/lib", "/lib/a.epub", "A", "9780141439518"),
		"/lib/claimed.epub": cand("/lib", "/lib/claimed.epub", "Claimed", ""),
		"/lib/bad.epub":     {Path: "/lib/bad.epub", Dir: "/lib", Title: "Bad", MetaErr: "drm"},
	}
	got := ebookOnlyCandidates(epubs, map[string]bool{"/lib/claimed.epub": true}, "/lib", nil)

	byPath := map[string]ScannedBook{}
	for _, sb := range got {
		byPath[sb.SourcePath] = sb
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2 (the claimed epub belongs to its audiobook)", len(got))
	}
	a := byPath["/lib/a.epub"]
	if a.Kind != string(state.KindEbook) || a.EbookPath != a.SourcePath {
		t.Errorf("ebook-only candidate = %+v; source and ebook path should be the same file", a)
	}
	if a.AudioFiles != 0 {
		t.Errorf("audio_files = %d, want 0", a.AudioFiles)
	}
	// An unreadable epub is REPORTED, not dropped - the user should see the book
	// they own and learn why it cannot be used.
	bad := byPath["/lib/bad.epub"]
	if bad.SourcePath == "" {
		t.Fatal("the unreadable epub was dropped instead of reported")
	}
	if bad.Kind == string(state.KindEbook) || bad.EbookNote == "" {
		t.Errorf("unreadable epub = %+v, want no kind and an explanatory note", bad)
	}
}
