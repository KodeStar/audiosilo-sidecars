package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// fakeBackend is a scripted asr.Backend for tests: it records each chapter it
// transcribes and writes a valid openai-format raw transcript, with optional hooks
// to observe/block a chapter (for cancel/resume tests). It never touches a real
// model or the network.
type fakeBackend struct {
	mu          sync.Mutex
	transcribed map[int]int // chapter -> transcribe count
	before      func(chapter int)
	block       chan struct{} // when non-nil, Transcribe waits on it (or ctx)
}

func newFakeBackend() *fakeBackend { return &fakeBackend{transcribed: map[int]int{}} }

func (f *fakeBackend) ID() string { return "fake" }

func (f *fakeBackend) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "fake", Available: true, Device: "cpu", Version: "fake"}, nil
}

func (f *fakeBackend) EnsureReady(context.Context, string) error { return nil }

func (f *fakeBackend) Transcribe(ctx context.Context, job asr.Job) error {
	if f.before != nil {
		f.before(job.Chapter)
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.block:
		}
	}
	f.mu.Lock()
	f.transcribed[job.Chapter]++
	f.mu.Unlock()
	raw := fmt.Sprintf(`{"text":" fake chapter %d","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" fake chapter %d","avg_logprob":NaN,"words":[{"word":" fake","start":0,"end":0.5,"probability":0.9}]}]}`, job.Chapter, job.Chapter)
	return os.WriteFile(filepath.Join(job.OutDir, transcript.RawName(job.Chapter)), []byte(raw+"\n"), 0o644) //nolint:gosec // test artifact
}

func (f *fakeBackend) count(chapter int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcribed[chapter]
}

// fakeASR builds an ASRSetup around a fake backend (available).
func fakeASR(b asr.Backend) ASRSetup {
	return ASRSetup{Backend: b, Cap: asr.Capability{Backend: "fake", Available: true, Device: "cpu"}, Model: "fake-model", Language: "en"}
}

// genFixtureM4B builds a tiny 3-chapter .m4b, skipping if ffmpeg is unavailable.
func genFixtureM4B(t *testing.T, ffmpeg, dir string) string {
	t.Helper()
	titles := []string{"Chapter 1: One", "Chapter 2: Two", "Chapter 3: Three"}
	const secs = 2
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Fixture Book\n")
	for i, title := range titles {
		meta.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		meta.WriteString("START=" + strconv.Itoa(i*secs*1000) + "\n")
		meta.WriteString("END=" + strconv.Itoa((i+1)*secs*1000) + "\n")
		meta.WriteString("title=" + title + "\n")
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	out := filepath.Join(dir, "book.m4b")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration="+strconv.Itoa(secs*len(titles)),
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1", "-c:a", "aac", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate fixture m4b: %v", err)
	}
	return out
}

// TestPipelineInspectSplitToDone drives a real tiny m4b through inspecting and
// splitting via the scheduler (stub executors beyond split), and asserts the
// manifest + FLACs land and the book advances to done.
func TestPipelineInspectSplitToDone(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	book := genFixtureM4B(t, ffmpeg, dir)
	workRoot := filepath.Join(dir, "work")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)

	fake := newFakeBackend()
	exe := NewExecutor(db, ffmpeg, ffprobe, dir, fakeASR(fake), scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond))
	sched := scheduler.New(db, hub, exe, 2, workRoot)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()

	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: book,
		WorkDir:    filepath.Join(workRoot, "fixture"),
		Title:      "Fixture Book",
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	sched.Notify()

	final := waitState(t, db, b.ID, "done", 30*time.Second)
	cancel()
	<-done

	if final.State != "done" {
		t.Fatalf("book state = %q (status %q, err %q), want done", final.State, final.Status, final.Error)
	}
	// Real artifacts from the mechanical stages.
	if _, err := audio.ReadManifest(b.WorkDir); err != nil {
		t.Errorf("manifest.json missing after pipeline: %v", err)
	}
	for i := 1; i <= 3; i++ {
		p := filepath.Join(b.WorkDir, audio.ChaptersDir, audio.ChapterFileName(i))
		if info, err := os.Stat(p); err != nil || info.Size() == 0 {
			t.Errorf("chapter %d FLAC missing/empty: %v", i, err)
		}
	}
	// ASR wrote each chapter's raw transcript (frozen 0444) and the sanitize stage
	// derived the normalized + text layers.
	for i := 1; i <= 3; i++ {
		if fake.count(i) != 1 {
			t.Errorf("chapter %d transcribed %d times, want 1", i, fake.count(i))
		}
		raw := filepath.Join(b.WorkDir, transcript.RawDir, transcript.RawName(i))
		info, err := os.Stat(raw)
		if err != nil {
			t.Errorf("raw transcript ch%03d missing: %v", i, err)
			continue
		}
		if perm := info.Mode().Perm(); perm != 0o444 {
			t.Errorf("raw transcript ch%03d perm = %o, want 444 (immutable)", i, perm)
		}
		if _, err := os.Stat(filepath.Join(b.WorkDir, transcript.JSONDir, transcript.JSONName(i))); err != nil {
			t.Errorf("normalized transcript ch%03d missing: %v", i, err)
		}
		txt, err := os.ReadFile(filepath.Join(b.WorkDir, transcript.TextDir, transcript.TextName(i)))
		if err != nil || len(txt) == 0 {
			t.Errorf("text transcript ch%03d missing/empty: %v", i, err)
		}
	}
	// Inspecting recorded a contiguous-markers decision in its sentinel.
	sn, err := scheduler.ReadSentinel(b.WorkDir, string(state.Inspecting))
	if err != nil || !sn.Result.MarkersContiguous {
		t.Errorf("inspecting sentinel = %+v, %v; want MarkersContiguous", sn.Result, err)
	}
	// The stages accounted the work dir's on-disk scratch into the persisted column.
	if final.ScratchBytes <= 0 {
		t.Errorf("scratch_bytes = %d after pipeline, want > 0", final.ScratchBytes)
	}
}

// waitState polls until the book reaches want or the deadline passes.
func waitState(t *testing.T, db *store.DB, id int64, want string, timeout time.Duration) store.Book {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := db.GetBook(context.Background(), id)
		if err == nil && (b.State == want || b.Status == string(state.StatusFailed)) {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := db.GetBook(context.Background(), id)
	return b
}

// waitStatus polls until the book carries want (a status flag) or the deadline
// passes.
func waitStatus(t *testing.T, db *store.DB, id int64, want string, timeout time.Duration) store.Book {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := db.GetBook(context.Background(), id)
		if err == nil && b.Status == want {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := db.GetBook(context.Background(), id)
	return b
}

// genM4BWithTitles builds a tiny .m4b whose chapter markers carry titles.
func genM4BWithTitles(t *testing.T, ffmpeg, dir string, titles []string) string {
	t.Helper()
	const secs = 2
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Fixture Book\n")
	for i, title := range titles {
		meta.WriteString("[CHAPTER]\nTIMEBASE=1/1000\n")
		meta.WriteString("START=" + strconv.Itoa(i*secs*1000) + "\n")
		meta.WriteString("END=" + strconv.Itoa((i+1)*secs*1000) + "\n")
		meta.WriteString("title=" + title + "\n")
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	out := filepath.Join(dir, "book.m4b")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration="+strconv.Itoa(secs*len(titles)),
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1", "-c:a", "aac", out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate m4b: %v", err)
	}
	return out
}

// TestPipelineParksUnnormalizableMarkers is the item-1 guard: a book whose markers
// are not a clean contiguous run (a gap), AND a markerless file (zero usable
// markers), both route to markers_normalizing and park needs_attention with the
// clear M5-deferral message - rather than the stub advancing them into a
// manifest-less split that fails misleadingly.
func TestPipelineParksUnnormalizableMarkers(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	// A non-contiguous marker set (1,2,4) and a markerless single file.
	gapDir := t.TempDir()
	gapBook := genM4BWithTitles(t, ffmpeg, gapDir, []string{"Chapter 1", "Chapter 2", "Chapter 4"})

	bareDir := t.TempDir()
	bareBook := filepath.Join(bareDir, "book.m4a")
	if out, cerr := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=3", "-c:a", "aac", bareBook).CombinedOutput(); cerr != nil {
		t.Fatalf("generate markerless m4a: %v: %s", cerr, out)
	}

	for _, tc := range []struct {
		name, src string
	}{
		{"non-contiguous markers", gapBook},
		{"markerless file", bareBook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			workRoot := filepath.Join(dir, "work")
			db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			hub := events.NewHub(1024)
			exe := NewExecutor(db, ffmpeg, ffprobe, dir, fakeASR(newFakeBackend()), scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond))
			sched := scheduler.New(db, hub, exe, 2, workRoot)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = sched.Start(ctx) }()

			b, err := db.CreateBook(context.Background(), store.NewBook{
				SourcePath: tc.src,
				WorkDir:    filepath.Join(workRoot, "fixture"),
				Title:      "Fixture",
			})
			if err != nil {
				t.Fatalf("create book: %v", err)
			}
			sched.Notify()

			final := waitStatus(t, db, b.ID, string(state.StatusNeedsAttention), 30*time.Second)
			cancel()
			<-done

			if final.Status != string(state.StatusNeedsAttention) {
				t.Fatalf("status = %q (state %q, err %q), want needs_attention", final.Status, final.State, final.Error)
			}
			if final.State != string(state.MarkersNormalizing) {
				t.Errorf("parked at state %q, want markers_normalizing", final.State)
			}
			if final.Error != MarkersNormalizingMsg {
				t.Errorf("park reason = %q, want %q", final.Error, MarkersNormalizingMsg)
			}
		})
	}
}

// writeManifest writes a minimal markers-style manifest with n chapters into
// workDir (no ffmpeg needed - the asr/sanitize stages consume the manifest, not
// the source).
func writeManifest(t *testing.T, workDir string, n int) {
	t.Helper()
	m := audio.Manifest{Source: "/x/book.m4b", Title: "Book", Style: audio.StyleMarkers, ChapterCount: n}
	for i := 1; i <= n; i++ {
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i, Start: float64(i - 1), End: float64(i), Duration: 1})
	}
	if err := audio.WriteManifest(workDir, m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// seedFLACs writes placeholder chapter FLACs so the asr stage's existence check
// passes (the fake backend does not read them).
func seedFLACs(t *testing.T, workDir string, n int) {
	t.Helper()
	dir := filepath.Join(workDir, audio.ChaptersDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if err := os.WriteFile(filepath.Join(dir, audio.ChapterFileName(i)), []byte("flac"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
}

// seedRawTranscript writes a valid raw transcript for a chapter (frozen 0444, as
// the asr stage leaves it) so a resume test can pre-complete some chapters.
func seedRawTranscript(t *testing.T, workDir string, chapter int) {
	t.Helper()
	dir := filepath.Join(workDir, transcript.RawDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"text":" pre %d","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" pre %d","words":[]}]}`, chapter, chapter)
	p := filepath.Join(dir, transcript.RawName(chapter))
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
}

// TestASRStageResumesSkippingCompleted pre-seeds 2 of 3 raw transcripts and runs
// the asr stage directly (no scheduler, no ffmpeg): only chapter 3 is transcribed,
// progress reaches 3/3, and every raw file ends up frozen 0444.
func TestASRStageResumesSkippingCompleted(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 3)
	seedFLACs(t, work, 3)
	seedRawTranscript(t, work, 1)
	seedRawTranscript(t, work, 2)

	fake := newFakeBackend()
	exe := NewExecutor(nil, "", "", t.TempDir(), fakeASR(fake), scheduler.NewStubExecutor(0, 0))
	book := store.Book{ID: 1, WorkDir: work}

	var lastDone, lastTotal int
	res, err := exe.Execute(context.Background(), book, state.ASR, func(done, total int) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if fake.count(1) != 0 || fake.count(2) != 0 {
		t.Errorf("pre-completed chapters were re-transcribed: c1=%d c2=%d", fake.count(1), fake.count(2))
	}
	if fake.count(3) != 1 {
		t.Errorf("chapter 3 transcribed %d times, want 1", fake.count(3))
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Errorf("final progress = %d/%d, want 3/3", lastDone, lastTotal)
	}
	// Immutability: every raw file is 0444.
	for i := 1; i <= 3; i++ {
		info, err := os.Stat(filepath.Join(work, transcript.RawDir, transcript.RawName(i)))
		if err != nil {
			t.Errorf("raw ch%03d missing: %v", i, err)
			continue
		}
		if perm := info.Mode().Perm(); perm != 0o444 {
			t.Errorf("raw ch%03d perm = %o, want 444", i, perm)
		}
	}
	// The stage wrote its sentinel and the provenance sidecar as its final actions.
	if len(res.Metrics) == 0 {
		t.Error("asr stage returned no metrics")
	}
	if !scheduler.SentinelExists(work, string(state.ASR)) {
		t.Error("asr sentinel missing after stage")
	}
	if _, err := os.Stat(filepath.Join(work, "asr.json")); err != nil {
		t.Errorf("asr.json provenance missing: %v", err)
	}
}

// TestASRStageUnavailableFails asserts a book fails clearly when no ASR backend is
// available, rather than silently advancing.
func TestASRStageUnavailableFails(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)
	exe := NewExecutor(nil, "", "", t.TempDir(), ASRSetup{Backend: nil, Cap: asr.Capability{Available: false, Detail: "no python3"}}, scheduler.NewStubExecutor(0, 0))
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, nil)
	if err == nil {
		t.Fatal("asr stage should fail when the backend is unavailable")
	}
	if !strings.Contains(err.Error(), "no python3") {
		t.Errorf("error = %v, want it to carry the unavailability detail", err)
	}
}

// TestSanitizeStageDerivesLayers runs the sanitize stage over seeded raw
// transcripts and asserts it produces normalized + text layers and is idempotent.
func TestSanitizeStageDerivesLayers(t *testing.T) {
	work := t.TempDir()
	seedRawTranscript(t, work, 1)
	seedRawTranscript(t, work, 2)
	// Provenance so the normalized transcripts carry a backend stamp.
	if err := writeASRProvenance(work, asrProvenance{Backend: "fake", Model: "m", Language: "en"}); err != nil {
		t.Fatal(err)
	}
	exe := NewExecutor(nil, "", "", t.TempDir(), ASRSetup{}, scheduler.NewStubExecutor(0, 0))
	book := store.Book{ID: 1, WorkDir: work}
	for pass := 0; pass < 2; pass++ { // idempotent: run twice
		if _, err := exe.Execute(context.Background(), book, state.Sanitizing, nil); err != nil {
			t.Fatalf("sanitize pass %d: %v", pass, err)
		}
	}
	for i := 1; i <= 2; i++ {
		raw, err := os.ReadFile(filepath.Join(work, transcript.JSONDir, transcript.JSONName(i)))
		if err != nil {
			t.Fatalf("normalized ch%03d: %v", i, err)
		}
		if !strings.Contains(string(raw), transcript.Schema) {
			t.Errorf("normalized ch%03d missing schema tag", i)
		}
		txt, err := os.ReadFile(filepath.Join(work, transcript.TextDir, transcript.TextName(i)))
		if err != nil || len(txt) == 0 {
			t.Errorf("text ch%03d missing/empty: %v", i, err)
		}
	}
}

// TestPipelineCancelMidASRResumes drives a book to the asr stage, blocks the fake
// backend after chapter 1, cancels mid-asr, then restarts with an unblocked
// backend and asserts completed chapters are not re-transcribed.
func TestPipelineCancelMidASRResumes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	book := genFixtureM4B(t, ffmpeg, dir)
	workRoot := filepath.Join(dir, "work")
	workDir := filepath.Join(workRoot, "fixture")

	db, err := store.Open(context.Background(), filepath.Join(dir, "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := events.NewHub(1024)

	// First run: the fake blocks on chapter 1 forever until we cancel.
	blocking := newFakeBackend()
	blocking.block = make(chan struct{})
	reached := make(chan struct{}, 1)
	blocking.before = func(int) {
		select {
		case reached <- struct{}{}:
		default:
		}
	}
	exe1 := NewExecutor(db, ffmpeg, ffprobe, dir, fakeASR(blocking), scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond))
	sched1 := scheduler.New(db, hub, exe1, 2, workRoot)
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { defer close(done1); _ = sched1.Start(ctx1) }()

	b, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: book, WorkDir: workDir, Title: "Fixture"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	sched1.Notify()

	// Wait until the asr stage has started transcribing (chapter 1 is blocked).
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("asr stage never started")
	}
	cancel1() // interrupts the blocked Transcribe (ctx path)
	<-done1

	// No raw transcript should have been finalized for the blocked chapter.
	if blocking.count(1) != 0 {
		t.Errorf("blocked chapter 1 was finalized %d times, want 0", blocking.count(1))
	}

	// Second run: a fresh, unblocked backend resumes the book to done.
	resume := newFakeBackend()
	exe2 := NewExecutor(db, ffmpeg, ffprobe, dir, fakeASR(resume), scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond))
	sched2 := scheduler.New(db, hub, exe2, 2, workRoot)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); _ = sched2.Start(ctx2) }()
	sched2.Notify()

	final := waitState(t, db, b.ID, "done", 30*time.Second)
	cancel2()
	<-done2

	if final.State != "done" {
		t.Fatalf("resumed book state = %q (status %q err %q), want done", final.State, final.Status, final.Error)
	}
	// Each chapter transcribed exactly once by the resuming backend (the split FLACs
	// survived, and no chapter had a completed raw from run 1).
	for i := 1; i <= 3; i++ {
		if resume.count(i) != 1 {
			t.Errorf("chapter %d transcribed %d times on resume, want 1", i, resume.count(i))
		}
	}
}
