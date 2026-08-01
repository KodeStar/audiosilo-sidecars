package pipeline

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/ebook"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// buildTestEpub writes a minimal but real epub: container, OPF, an EPUB3 nav, and
// one xhtml per entry. chapters are (label, body) pairs in spine order.
func buildTestEpub(t *testing.T, path string, chapters [][2]string) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("create epub: %v", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)

	write := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var items, spine, nav strings.Builder
	for i, c := range chapters {
		id := "c" + strconv.Itoa(i+1)
		href := id + ".xhtml"
		items.WriteString(`<item id="` + id + `" href="` + href + `" media-type="application/xhtml+xml"/>`)
		spine.WriteString(`<itemref idref="` + id + `"/>`)
		nav.WriteString(`<li><a href="` + href + `">` + c[0] + `</a></li>`)
		write("OEBPS/"+href, "<html><body><p>"+c[1]+"</p></body></html>")
	}

	write("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
	write("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>A Test Book</dc:title><dc:creator>A Writer</dc:creator><dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>`+items.String()+`</manifest>
  <spine>`+spine.String()+`</spine>
</package>`)
	write("OEBPS/nav.xhtml", `<html xmlns:epub="http://www.idpf.org/2007/ops"><body>
  <nav epub:type="toc"><ol>`+nav.String()+`</ol></nav></body></html>`)

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
}

func words(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }

func TestExtractStageContiguous(t *testing.T) {
	dir := t.TempDir()
	epub := filepath.Join(dir, "book.epub")
	buildTestEpub(t, epub, [][2]string{
		{"Cover", words(3)},
		{"Chapter 1: The Start", words(1200)},
		{"Chapter 2: The Middle", words(1300)},
		{"Chapter 3: The End", words(1100)},
		{"About the Author", words(20)},
	})

	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	e := &Executor{}
	book := store.Book{ID: 1, Title: "A Test Book", WorkDir: workDir, Kind: "ebook", EbookPath: epub}

	var noted []string
	res, err := e.extract(context.Background(), book, scheduler.StageReport{
		Note: func(s string) { noted = append(noted, s) },
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !res.ChaptersMapped {
		t.Fatalf("ChaptersMapped = false; a clean numbered toc must skip chapter_mapping (notes: %v)", noted)
	}

	// The shared chapter-universe contract the authoring tail reads.
	man, err := audio.ReadManifest(workDir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if man.Style != audio.StyleEbook || man.ChapterCount != 3 || man.Duration != 0 {
		t.Errorf("manifest style/count/duration = %q/%d/%v, want ebook/3/0", man.Style, man.ChapterCount, man.Duration)
	}
	if len(man.Chapters) != 3 || man.Chapters[0].Words == 0 {
		t.Errorf("manifest chapters = %+v, want 3 with word counts", man.Chapters)
	}

	// One text file per LOGICAL chapter, named so chapter N is chNNN.txt.
	for i, want := range []string{"ch001.txt", "ch002.txt", "ch003.txt"} {
		p := filepath.Join(workDir, ebook.TextDir, want)
		body, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatalf("chapter %d text: %v", i+1, err)
		}
		if len(strings.Fields(string(body))) < 1000 {
			t.Errorf("%s has %d words, want the chapter body", want, len(strings.Fields(string(body))))
		}
	}
	// Front and back matter must NOT have become chapters.
	if _, err := os.Stat(filepath.Join(workDir, ebook.TextDir, "ch004.txt")); err == nil {
		t.Error("a fourth chapter file exists; front/back matter leaked into the chapter universe")
	}

	// The chunk plan is written here because spelling_research never runs for an ebook.
	if _, err := loadChunkPlan(workDir); err != nil {
		t.Errorf("chunk plan: %v", err)
	}
	// And the audit trail the mapping agent and a human read.
	if _, err := ebook.ReadManifest(workDir); err != nil {
		t.Errorf("extract manifest: %v", err)
	}
	// The sentinel must record the routing decision for crash-resume.
	raw, err := os.ReadFile(filepath.Join(workDir, "_done", "extracting.json")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	var sent struct {
		Result scheduler.StageResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil || !sent.Result.ChaptersMapped {
		t.Errorf("sentinel = %s (err %v), want chapters_mapped true", raw, err)
	}
}

// TestExtractStageRoutesToMappingWhenNotContiguous: a part-restarting book yields
// duplicate chapter numbers, so it must reach the agent rather than publish
// overlapping spoiler positions - and must NOT write chapter text, since the
// numbering it would use is the thing in doubt.
func TestExtractStageRoutesToMappingWhenNotContiguous(t *testing.T) {
	dir := t.TempDir()
	epub := filepath.Join(dir, "book.epub")
	buildTestEpub(t, epub, [][2]string{
		{"Part I", words(5)},
		{"Chapter 1", words(900)},
		{"Chapter 2", words(900)},
		{"Part II", words(5)},
		{"Chapter 1", words(900)},
		{"Chapter 2", words(900)},
	})
	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	e := &Executor{}
	book := store.Book{ID: 1, WorkDir: workDir, Kind: "ebook", EbookPath: epub}

	res, err := e.extract(context.Background(), book, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.ChaptersMapped {
		t.Error("ChaptersMapped = true for a part-restarting book; duplicate positions must not be published")
	}
	if _, err := os.Stat(filepath.Join(workDir, ebook.TextDir)); err == nil {
		t.Error("chapter text was written despite an untrusted numbering")
	}
	// The draft IS recorded, so chapter_mapping has something to correct.
	if _, err := ebook.ReadManifest(workDir); err != nil {
		t.Errorf("extract manifest missing: %v", err)
	}
}

func TestExtractStageParks(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	e := &Executor{}

	// Unreadable: not an epub at all.
	bad := filepath.Join(dir, "bad.epub")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.extract(context.Background(), store.Book{ID: 1, WorkDir: workDir, Kind: "ebook", EbookPath: bad},
		scheduler.StageReport{})
	assertPark(t, err, state.ParkEbookUnreadable)

	// No chapters: a single unlabeled blob has nothing an agent could map either.
	blob := filepath.Join(dir, "blob.epub")
	buildTestEpub(t, blob, [][2]string{{"", words(5000)}})
	work2 := filepath.Join(dir, "work2")
	if err := os.MkdirAll(work2, 0o750); err != nil {
		t.Fatal(err)
	}
	_, err = e.extract(context.Background(), store.Book{ID: 2, WorkDir: work2, Kind: "ebook", EbookPath: blob},
		scheduler.StageReport{})
	assertPark(t, err, state.ParkEbookNoChapters)
}

// TestExtractStageRefusesAnAudioBook is the routing-regression guard: books.kind
// picks the front half, so handing an audiobook folder to the epub reader means the
// two disagree, and that must be a loud failure rather than a confusing zip error.
func TestExtractStageRefusesAnAudioBook(t *testing.T) {
	e := &Executor{}
	_, err := e.extract(context.Background(),
		store.Book{ID: 1, WorkDir: t.TempDir(), Kind: "audio", SourcePath: "/books/some-audiobook"},
		scheduler.StageReport{})
	if err == nil || !strings.Contains(err.Error(), "only an ebook book can be extracted") {
		t.Errorf("err = %v, want a refusal naming the kind mismatch", err)
	}
}
