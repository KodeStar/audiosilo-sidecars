package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
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
	mu            sync.Mutex
	transcribed   map[int]int // chapter -> transcribe count
	before        func(chapter int)
	block         chan struct{} // when non-nil, Transcribe waits on it (or ctx)
	transcribeErr error         // when non-nil, Transcribe returns it (a real failure)
	ensureErr     error         // when non-nil, EnsureReady returns it (an environment precondition)
	// emptyMode scripts per-chapter empty output across attempts: "" normal (always
	// non-empty), "once" empty on the first attempt then non-empty, "always" empty on
	// every attempt. An empty attempt writes a valid-but-segmentless raw transcript.
	emptyMode string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{transcribed: map[int]int{}} }

func (f *fakeBackend) ID() string { return "fake" }

func (f *fakeBackend) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "fake", Available: true, Device: "cpu", Version: "fake"}, nil
}

func (f *fakeBackend) EnsureReady(context.Context) error { return f.ensureErr }

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
	if f.transcribeErr != nil {
		return f.transcribeErr
	}
	f.mu.Lock()
	f.transcribed[job.Chapter]++
	attempt := f.transcribed[job.Chapter] // 1-based attempt count for this chapter
	mode := f.emptyMode
	f.mu.Unlock()

	empty := mode == "always" || (mode == "once" && attempt == 1)
	var raw string
	if empty {
		// Structurally complete (passes transcript.Complete) but segmentless - the
		// empty-transcript failure mode the asr stage must double-check.
		raw = `{"text":"","language":"en","segments":[]}`
	} else {
		raw = fmt.Sprintf(`{"text":" fake chapter %d","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" fake chapter %d","avg_logprob":NaN,"words":[{"word":" fake","start":0,"end":0.5,"probability":0.9}]}]}`, job.Chapter, job.Chapter)
	}
	// Both real backends name the raw <audio-stem>.json - hand-mirrored here (not via
	// asr.RawOutputName) so a wrapper or helper that drifts from the tools' naming fails
	// this test.
	stem := strings.TrimSuffix(filepath.Base(job.Audio), filepath.Ext(job.Audio))
	return os.WriteFile(filepath.Join(job.OutDir, stem+".json"), []byte(raw+"\n"), 0o644) //nolint:gosec // test artifact
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
	// A full fake agent drives the M5 agent stages (spelling_research onward) so the book
	// runs the whole pipeline to done; the mechanical stages before it stay real.
	fakeAgent := newFakeRunner()
	fakeAgent.act = fullFakeAct(t, fullFakeOpts{title: "Fixture Book"})
	cfg := fullFakeConfig(dir, fakeAgent)
	cfg.DB, cfg.FFmpeg, cfg.FFprobe, cfg.ASR = db, ffmpeg, ffprobe, fakeASR(fake)
	cfg.Fallback = scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	exe := NewExecutor(cfg)
	sched := scheduler.New(db, hub, exe, 2, workRoot, false)

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

// TestPipelineParksUnnormalizableMarkers is the routing guard: a book whose markers
// are not a clean contiguous run (a gap), AND a markerless file (zero usable
// markers), both route to markers_normalizing - rather than the stub advancing them
// into a manifest-less split that fails misleadingly. With no agent backend available,
// the stage parks needs_attention with AgentUnavailableMsg (an actionable,
// Retry-able precondition); the marker-mapping itself is exercised by the agent-stage
// tests with a fake runner.
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
			exe := NewExecutor(Config{DB: db, FFmpeg: ffmpeg, FFprobe: ffprobe, DataDir: dir, ASR: fakeASR(newFakeBackend()), Fallback: scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)})
			// This machine may have a real claude CLI; force the agent unavailable so the
			// stage parks deterministically on the routing, not on a live agent run.
			exe.redetectAgent = func(context.Context) (agent.Runner, agent.Availability) {
				return nil, agent.Availability{Detail: "no agent CLI found"}
			}
			sched := scheduler.New(db, hub, exe, 2, workRoot, false)
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
			if final.Error != AgentUnavailableMsg {
				t.Errorf("park reason = %q, want %q", final.Error, AgentUnavailableMsg)
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
// TestSplitRejectsNonContiguousManifest is the item-5 regression: the split stage
// fails loudly on a non-contiguous manifest rather than cutting FLACs at credit/sample
// boundaries (the last-line guard restored after inspect began writing draft manifests
// for non-contiguous markers). ffmpeg is a dummy non-empty path so the contiguity
// check is reached but never executed.
func TestSplitRejectsNonContiguousManifest(t *testing.T) {
	work := t.TempDir()
	writeManifestStruct(t, work, audio.Manifest{Source: "/x", Style: audio.StyleMarkers, Duration: 6, ChapterCount: 3, Chapters: markerChapters(1, 2, 4)})
	exe := NewExecutor(Config{FFmpeg: "/usr/bin/true", DataDir: t.TempDir(), Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.Splitting, scheduler.StageReport{})
	if err == nil {
		t.Fatal("split accepted a non-contiguous manifest, want a loud error")
	}
	if !strings.Contains(err.Error(), "not contiguous") {
		t.Errorf("error = %v, want it to mention non-contiguity", err)
	}
	if scheduler.SentinelExists(work, string(state.Splitting)) {
		t.Error("split wrote a sentinel despite the contiguity error")
	}
}

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
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	book := store.Book{ID: 1, WorkDir: work}

	var lastDone, lastTotal int
	res, err := exe.Execute(context.Background(), book, state.ASR, scheduler.StageReport{Progress: func(done, total int) {
		lastDone, lastTotal = done, total
	}})
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

// TestASRStageResumeRateSampleUnits asserts the RateSample a resumed ASR run reports
// counts ONLY the chapters transcribed this run (1 of 3), not the whole book, so a
// resume can never corrupt the learned per-chapter rate with a whole-book count.
func TestASRStageResumeRateSampleUnits(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 3)
	seedFLACs(t, work, 3)
	seedRawTranscript(t, work, 1)
	seedRawTranscript(t, work, 2)

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{Progress: func(int, int) {}})
	if err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if res.RateSample == nil {
		t.Fatal("asr stage reported no RateSample; want one for the chapter it transcribed")
	}
	if res.RateSample.Units != 1 {
		t.Errorf("RateSample.Units = %d, want 1 (only chapter 3 transcribed this run, not all 3)", res.RateSample.Units)
	}
	if res.RateSample.Seconds <= 0 {
		t.Errorf("RateSample.Seconds = %v, want > 0", res.RateSample.Seconds)
	}
}

// TestASRStageUnavailableParks asserts a book PARKS needs_attention (not a hard
// failure) when no ASR backend is available, carrying the capability detail so a
// human knows what to fix before retrying.
func TestASRStageUnavailableParks(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{Backend: nil, Cap: asr.Capability{Available: false, Detail: "no python3"}}, Fallback: scheduler.NewStubExecutor(0, 0)})
	// Re-detection also finds nothing (this machine may have a real backend, which is
	// not what this test is about), so the stage parks on the unavailable capability.
	exe.redetectASR = func(context.Context) (asr.Backend, asr.Capability, string) {
		return nil, asr.Capability{}, ""
	}
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("asr stage error = %v, want a ParkError (needs_attention)", err)
	}
	if !strings.Contains(pe.Reason, "no python3") {
		t.Errorf("park reason = %q, want it to carry the unavailability detail", pe.Reason)
	}
}

// TestASRStageEnsureReadyFailureParks asserts an EnsureReady failure PARKS the
// book needs_attention with actionable guidance rather than hard-failing it: with
// auto-download on, Detect is optimistic (available=true, no network I/O), so a
// fresh offline box passes the availability gate and first trips at EnsureReady's
// binary/model fetch - an environment/tooling precondition, never a book-content
// error, and Retry must be able to re-admit the book once the human fixes it.
func TestASRStageEnsureReadyFailureParks(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)
	fake := newFakeBackend()
	fake.ensureErr = errors.New("whisper-cli download: dial tcp: no route to host")
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("asr stage error = %v, want a ParkError (needs_attention)", err)
	}
	if !strings.Contains(pe.Reason, "ASR setup failed") ||
		!strings.Contains(pe.Reason, "no route to host") ||
		!strings.Contains(pe.Reason, "retry") {
		t.Errorf("park reason = %q, want the setup-failed guidance carrying the underlying cause", pe.Reason)
	}
}

// TestASRStageTranscribeFailureFails asserts a genuine transcription error (an
// available backend whose Transcribe fails) is a HARD failure, not a park - only a
// missing precondition parks.
func TestASRStageTranscribeFailureFails(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)
	fake := newFakeBackend()
	fake.transcribeErr = errors.New("model exploded")
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{})
	if err == nil {
		t.Fatal("a transcription failure should fail the stage")
	}
	var pe *scheduler.ParkError
	if errors.As(err, &pe) {
		t.Errorf("a transcription failure must not park; got ParkError %q", pe.Reason)
	}
}

// TestStagesParkWhenMediaToolsMissing asserts inspect (no ffprobe) and split (no
// ffmpeg) PARK needs_attention with the media-tools message, rather than
// hard-failing, since a missing tool is a human-fixable startup precondition.
func TestStagesParkWhenMediaToolsMissing(t *testing.T) {
	work := t.TempDir()
	// Unresolved tools: unset resolved paths AND explicit config paths that point at
	// nonexistent binaries, so the local re-resolution (which honors an explicit path
	// exactly, never falling back to $PATH) still finds nothing on a machine that has
	// ffmpeg/ffprobe installed.
	nope := t.TempDir()
	exe := NewExecutor(Config{
		DataDir:  t.TempDir(),
		Tools:    ToolConfig{FFmpegPath: filepath.Join(nope, "no-ffmpeg"), FFprobePath: filepath.Join(nope, "no-ffprobe")},
		ASR:      fakeASR(newFakeBackend()),
		Fallback: scheduler.NewStubExecutor(0, 0),
	})
	book := store.Book{ID: 1, WorkDir: work, SourcePath: filepath.Join(work, "book.m4b")}
	for _, stage := range []state.State{state.Inspecting, state.Splitting} {
		_, err := exe.Execute(context.Background(), book, stage, scheduler.StageReport{})
		var pe *scheduler.ParkError
		if !errors.As(err, &pe) {
			t.Fatalf("stage %s error = %v, want a ParkError", stage, err)
		}
		if pe.Reason != MediaToolsUnavailableMsg {
			t.Errorf("stage %s park reason = %q, want %q", stage, pe.Reason, MediaToolsUnavailableMsg)
		}
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
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{}, Fallback: scheduler.NewStubExecutor(0, 0)})
	book := store.Book{ID: 1, WorkDir: work}
	for pass := range 2 { // idempotent: run twice
		if _, err := exe.Execute(context.Background(), book, state.Sanitizing, scheduler.StageReport{}); err != nil {
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
	exe1 := NewExecutor(Config{DB: db, FFmpeg: ffmpeg, FFprobe: ffprobe, DataDir: dir, ASR: fakeASR(blocking), Fallback: scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)})
	sched1 := scheduler.New(db, hub, exe1, 2, workRoot, false)
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

	// Second run: a fresh, unblocked backend resumes the book to done. The full fake
	// agent drives the M5 agent stages so the resumed book reaches done.
	resume := newFakeBackend()
	fakeAgent := newFakeRunner()
	fakeAgent.act = fullFakeAct(t, fullFakeOpts{title: "Fixture"})
	cfg2 := fullFakeConfig(dir, fakeAgent)
	cfg2.DB, cfg2.FFmpeg, cfg2.FFprobe, cfg2.ASR = db, ffmpeg, ffprobe, fakeASR(resume)
	cfg2.Fallback = scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	exe2 := NewExecutor(cfg2)
	sched2 := scheduler.New(db, hub, exe2, 2, workRoot, false)
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

// TestASRReDetectsBackendOnRetry asserts the asr stage re-selects an ASR backend at
// stage entry: a book parks when none is available, then the SAME executor resumes
// (no daemon restart) once a backend appears - the retry-after-install path.
func TestASRReDetectsBackendOnRetry(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)

	fake := newFakeBackend()
	var available bool
	exe := NewExecutor(Config{
		DataDir:  t.TempDir(),
		ASR:      ASRSetup{Backend: nil, Cap: asr.Capability{Available: false, Detail: "no backend"}},
		Fallback: scheduler.NewStubExecutor(0, 0),
	})
	exe.redetectASR = func(context.Context) (asr.Backend, asr.Capability, string) {
		if !available {
			return nil, asr.Capability{}, ""
		}
		return fake, asr.Capability{Backend: "fake", Available: true, Device: "cpu"}, "fake-model"
	}
	book := store.Book{ID: 1, WorkDir: work}

	// No backend yet -> park, and nothing transcribed.
	_, err := exe.Execute(context.Background(), book, state.ASR, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("first run error = %v, want a ParkError", err)
	}
	if fake.count(1) != 0 {
		t.Fatalf("chapter transcribed %d times before a backend was available", fake.count(1))
	}

	// A backend appears; the same executor now runs to completion without a restart.
	available = true
	if _, err := exe.Execute(context.Background(), book, state.ASR, scheduler.StageReport{}); err != nil {
		t.Fatalf("second run after backend appeared: %v", err)
	}
	if fake.count(1) != 1 {
		t.Errorf("chapter transcribed %d times after backend appeared, want 1", fake.count(1))
	}
}

// TestEnsureToolsAdoptsAppearingTool asserts ensureTools re-resolves a media tool
// that only appears after startup (explicit config path honored), and that
// ToolPaths reflects the adoption - without driving a real stage.
func TestEnsureToolsAdoptsAppearingTool(t *testing.T) {
	dir := t.TempDir()
	toolPath := filepath.Join(dir, "ffprobe-fake")
	exe := NewExecutor(Config{
		FFprobe:  "",
		Tools:    ToolConfig{FFprobePath: toolPath},
		Fallback: scheduler.NewStubExecutor(0, 0),
	})
	if _, fp := exe.ToolPaths(); fp != "" {
		t.Fatalf("ffprobe path = %q before the tool exists, want empty", fp)
	}
	if _, fp := exe.ensureTools(); fp != "" {
		t.Fatalf("ensureTools resolved %q before the tool exists, want empty", fp)
	}
	// The operator installs the binary (executable).
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	if _, fp := exe.ensureTools(); fp != toolPath {
		t.Errorf("ensureTools resolved %q after creation, want %q", fp, toolPath)
	}
	if _, fp := exe.ToolPaths(); fp != toolPath {
		t.Errorf("ToolPaths = %q after adoption, want %q", fp, toolPath)
	}
}

// TestASRResumesOnMatchingFingerprint asserts a resume whose recorded manifest
// fingerprint still matches proceeds normally (skips completed chapters), NOT parks.
func TestASRResumesOnMatchingFingerprint(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 3)
	seedFLACs(t, work, 3)
	seedRawTranscript(t, work, 1)
	seedRawTranscript(t, work, 2)
	fp, err := manifestFingerprint(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeASRProvenance(work, asrProvenance{Backend: "fake", Model: "m", Language: "en", ManifestSHA: fp}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{}); err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if fake.count(1) != 0 || fake.count(2) != 0 {
		t.Errorf("pre-completed chapters re-transcribed: c1=%d c2=%d", fake.count(1), fake.count(2))
	}
	if fake.count(3) != 1 {
		t.Errorf("chapter 3 transcribed %d times, want 1", fake.count(3))
	}
}

// TestASRParksOnManifestMismatch asserts a resume whose recorded fingerprint no
// longer matches the manifest PARKS (a different edition) and does NOT delete the
// existing raw evidence.
func TestASRParksOnManifestMismatch(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 3)
	seedFLACs(t, work, 3)
	seedRawTranscript(t, work, 1)
	if err := writeASRProvenance(work, asrProvenance{Backend: "fake", Model: "m", Language: "en", ManifestSHA: "deadbeef"}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{})
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a ParkError", err)
	}
	if pe.Reason != ManifestChangedMsg {
		t.Errorf("park reason = %q, want %q", pe.Reason, ManifestChangedMsg)
	}
	// The pre-seeded raw must survive - it is 0444 evidence, never silently deleted.
	if _, serr := os.Stat(filepath.Join(work, transcript.RawDir, transcript.RawName(1))); serr != nil {
		t.Errorf("pre-seeded raw was removed on park: %v", serr)
	}
}

// TestASRResumeReFreezesRaw asserts a completed raw left at 0644 (a crash between
// write and chmod) is re-frozen to 0444 on resume and NOT re-transcribed.
func TestASRResumeReFreezesRaw(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)
	rawDir := filepath.Join(work, transcript.RawDir)
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		t.Fatal(err)
	}
	raw := `{"text":" hi","language":"en","segments":[{"id":0,"start":0,"end":1,"text":" hi","words":[]}]}`
	rawPath := filepath.Join(rawDir, transcript.RawName(1))
	if err := os.WriteFile(rawPath, []byte(raw), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}

	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{}); err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if fake.count(1) != 0 {
		t.Errorf("completed chapter re-transcribed %d times, want 0 (skipped)", fake.count(1))
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Errorf("raw perm = %o after resume, want 444 (re-frozen)", perm)
	}
}

// TestASREmptyThenNonEmptyRetried asserts an empty transcript is deleted and
// retried once, the retry's non-empty result is adopted, and no chapter is recorded
// as accepted-empty.
func TestASREmptyThenNonEmptyRetried(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)

	fake := newFakeBackend()
	fake.emptyMode = "once"
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{}); err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if fake.count(1) != 2 {
		t.Errorf("chapter transcribed %d times, want 2 (empty then retry)", fake.count(1))
	}
	empty, err := rawIsEmpty(filepath.Join(work, transcript.RawDir, transcript.RawName(1)))
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Error("final raw is empty; want the retried non-empty transcript")
	}
	if prov := readASRProvenance(work); len(prov.EmptyChapters) != 0 {
		t.Errorf("empty_chapters = %v, want none", prov.EmptyChapters)
	}
}

// TestASREmptyTwiceAccepted asserts an empty transcript that stays empty across the
// retry is accepted (no park/error), frozen 0444, and recorded in asr.json's
// empty_chapters.
func TestASREmptyTwiceAccepted(t *testing.T) {
	work := t.TempDir()
	writeManifest(t, work, 1)
	seedFLACs(t, work, 1)

	fake := newFakeBackend()
	fake.emptyMode = "always"
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{}); err != nil {
		t.Fatalf("asr stage (empty accepted): %v", err)
	}
	if fake.count(1) != 2 {
		t.Errorf("chapter transcribed %d times, want 2 (one retry then accept)", fake.count(1))
	}
	rawPath := filepath.Join(work, transcript.RawDir, transcript.RawName(1))
	info, err := os.Stat(rawPath)
	if err != nil {
		t.Fatalf("accepted raw missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Errorf("accepted empty raw perm = %o, want 444", perm)
	}
	data, err := os.ReadFile(filepath.Join(work, "asr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prov struct {
		EmptyChapters []int `json:"empty_chapters"`
	}
	if err := json.Unmarshal(data, &prov); err != nil {
		t.Fatal(err)
	}
	if len(prov.EmptyChapters) != 1 || prov.EmptyChapters[0] != 1 {
		t.Errorf("empty_chapters = %v, want [1]", prov.EmptyChapters)
	}
}

// writeQAManifest writes a markers-style manifest whose chapters carry the supplied
// per-chapter durations (seconds), so the qa_sweep stage reads real durations for its
// words-per-hour and mid-chapter-position math. No ffmpeg needed.
func writeQAManifest(t *testing.T, work string, durations map[int]float64) {
	t.Helper()
	nums := make([]int, 0, len(durations))
	for n := range durations {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	m := audio.Manifest{Source: "/x/book.m4b", Title: "Book", Style: audio.StyleMarkers, ChapterCount: len(nums)}
	for _, n := range nums {
		d := durations[n]
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: n, Start: 0, End: d, Duration: d})
	}
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writeQATranscript writes one normalized transcripts-json/chNNN.json with the given
// segments, so the qa detectors have real input without an ASR/sanitize run.
func writeQATranscript(t *testing.T, work string, chapter int, segs []transcript.Segment) {
	t.Helper()
	tr := transcript.Transcript{
		Schema: transcript.Schema, Chapter: chapter, Backend: "fake", Model: "m", Language: "en", Segments: segs,
	}
	if err := transcript.WriteNormalized(filepath.Join(work, transcript.JSONDir), tr); err != nil {
		t.Fatalf("write transcript ch%d: %v", chapter, err)
	}
}

// cleanSegs is a short, distinct, well-behaved chapter body: three different
// short segments trip no detector (too few tokens for the 6-gram/tail detectors, no
// repeated run). Reused across chapters so their words-per-hour is identical (sd 0),
// which keeps the wph outlier detector quiet too.
func cleanSegs() []transcript.Segment {
	return []transcript.Segment{
		{ID: 0, Start: 0, End: 2, Text: " Hello there reader"},
		{ID: 1, Start: 2, End: 4, Text: " the story begins now"},
		{ID: 2, Start: 4, End: 6, Text: " onward we go swiftly"},
	}
}

// TestQASweepCleanBranch runs the qa_sweep stage over three clean chapters and
// asserts it reports QAClean, writes both reports, and records qa_clean in its
// sentinel. The real artifacts (which the stub never writes) prove the stage is
// wired, not falling through to the fallback.
func TestQASweepCleanBranch(t *testing.T) {
	work := t.TempDir()
	writeQAManifest(t, work, map[int]float64{1: 600, 2: 600, 3: 600})
	for _, ch := range []int{1, 2, 3} {
		writeQATranscript(t, work, ch, cleanSegs())
	}

	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{}, Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.QASweep, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_sweep stage: %v", err)
	}
	if !res.QAClean {
		t.Errorf("QAClean = false on a clean book, want true")
	}
	sn, err := scheduler.ReadSentinel(work, string(state.QASweep))
	if err != nil {
		t.Fatalf("read qa_sweep sentinel: %v", err)
	}
	if !sn.Result.QAClean {
		t.Errorf("sentinel qa_clean = false, want true")
	}
	for _, name := range []string{"qa_report.json", "qa_report.md"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Errorf("%s missing after qa_sweep: %v", name, err)
		}
	}
}

// TestQASweepDirtyBranch gives one chapter a mid-chapter repeated-segment loop (four
// identical segments starting at t=0, far below the 85% end-fade cutoff for a 600s
// chapter) and asserts the sweep reports QAClean=false and queues the chapter for
// re-transcription.
func TestQASweepDirtyBranch(t *testing.T) {
	work := t.TempDir()
	writeQAManifest(t, work, map[int]float64{1: 600, 2: 600, 3: 600})
	writeQATranscript(t, work, 1, cleanSegs())
	writeQATranscript(t, work, 2, []transcript.Segment{
		{ID: 0, Start: 0, End: 1, Text: " and then"},
		{ID: 1, Start: 1, End: 2, Text: " and then"},
		{ID: 2, Start: 2, End: 3, Text: " and then"},
		{ID: 3, Start: 3, End: 4, Text: " and then"},
	})
	writeQATranscript(t, work, 3, cleanSegs())

	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{}, Fallback: scheduler.NewStubExecutor(0, 0)})
	res, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.QASweep, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("qa_sweep stage: %v", err)
	}
	if res.QAClean {
		t.Errorf("QAClean = true on a book with a mid-chapter loop, want false")
	}
	raw, err := os.ReadFile(filepath.Join(work, "qa_report.json"))
	if err != nil {
		t.Fatalf("read qa_report.json: %v", err)
	}
	var rep struct {
		RetranscribeQueue []int `json:"retranscribe_queue"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.RetranscribeQueue) == 0 {
		t.Errorf("retranscribe_queue empty, want the looped chapter queued")
	}
}

// TestQASweepMissingTranscriptsErrors asserts qa_sweep is a loud, ordered-run error
// (naming sanitizing) when the normalized transcripts are absent - a manifest exists
// but the sanitizing stage never produced transcripts-json/.
func TestQASweepMissingTranscriptsErrors(t *testing.T) {
	work := t.TempDir()
	writeQAManifest(t, work, map[int]float64{1: 600})

	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{}, Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.QASweep, scheduler.StageReport{})
	if err == nil {
		t.Fatal("qa_sweep with no transcripts should error")
	}
	if !strings.Contains(err.Error(), "sanitizing") {
		t.Errorf("error = %q, want it to name the sanitizing stage", err)
	}
	var pe *scheduler.ParkError
	if errors.As(err, &pe) {
		t.Errorf("a missing-transcripts error must not park; got ParkError %q", pe.Reason)
	}
}

// TestQASweepMissingManifestErrors asserts qa_sweep errors (naming inspect) when the
// manifest is absent - it needs chapter durations, which only inspect produces.
func TestQASweepMissingManifestErrors(t *testing.T) {
	work := t.TempDir()
	// transcripts present but no manifest, so the manifest read is the first failure.
	writeQATranscript(t, work, 1, cleanSegs())

	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: ASRSetup{}, Fallback: scheduler.NewStubExecutor(0, 0)})
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.QASweep, scheduler.StageReport{})
	if err == nil {
		t.Fatal("qa_sweep with no manifest should error")
	}
	if !strings.Contains(err.Error(), "inspect") {
		t.Errorf("error = %q, want it to name the inspect stage", err)
	}
}

// TestASRStageResumeProgressBaseline is the M6 resume-baseline regression: on a resume
// with K of N chapters already transcribed, the FIRST progress report reflects the K
// already-complete chapters (not 0) and the counter ends at N, never dipping below the
// baseline or re-counting a skipped chapter. This makes the scheduler's EWMA unit span
// (first..last reported done) measure only the N-K chapters THIS run transcribed, so a
// resumed run does not inflate the learned per-chapter rate.
func TestASRStageResumeProgressBaseline(t *testing.T) {
	work := t.TempDir()
	const n, k = 5, 3
	writeManifest(t, work, n)
	seedFLACs(t, work, n)
	for i := 1; i <= k; i++ {
		seedRawTranscript(t, work, i)
	}
	exe := NewExecutor(Config{DataDir: t.TempDir(), ASR: fakeASR(newFakeBackend()), Fallback: scheduler.NewStubExecutor(0, 0)})

	var reports []int
	_, err := exe.Execute(context.Background(), store.Book{ID: 1, WorkDir: work}, state.ASR, scheduler.StageReport{Progress: func(done, total int) {
		if total != n {
			t.Errorf("total = %d, want %d", total, n)
		}
		reports = append(reports, done)
	}})
	if err != nil {
		t.Fatalf("asr stage: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("no progress reports")
	}
	if reports[0] != k {
		t.Errorf("first report done = %d, want the already-complete baseline %d", reports[0], k)
	}
	if last := reports[len(reports)-1]; last != n {
		t.Errorf("last report done = %d, want %d", last, n)
	}
	for i, d := range reports {
		if d < k {
			t.Errorf("report[%d] = %d dipped below the baseline %d", i, d, k)
		}
		if i > 0 && d < reports[i-1] {
			t.Errorf("report[%d] = %d < previous %d (progress not monotonic)", i, d, reports[i-1])
		}
	}
}
