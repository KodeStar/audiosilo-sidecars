package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

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

	exe := NewExecutor(ffmpeg, ffprobe, scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond))
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
	// Inspecting recorded a contiguous-markers decision in its sentinel.
	sn, err := scheduler.ReadSentinel(b.WorkDir, string(state.Inspecting))
	if err != nil || !sn.Result.MarkersContiguous {
		t.Errorf("inspecting sentinel = %+v, %v; want MarkersContiguous", sn.Result, err)
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
