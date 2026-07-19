package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/events"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

type supervisorRunnerStub struct {
	id      string
	noTools bool
}

func (r supervisorRunnerStub) ID() string { return r.id }
func (r supervisorRunnerStub) Detect(context.Context) agent.Availability {
	return agent.Availability{Backend: r.id, Available: true}
}
func (r supervisorRunnerStub) Run(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{}, nil
}
func (r supervisorRunnerStub) SupportsWeb() bool     { return false }
func (r supervisorRunnerStub) EnforcesNoTools() bool { return r.noTools }

func TestEmptySupervisorBackendNeverChangesProvider(t *testing.T) {
	codex := supervisorRunnerStub{id: agent.IDCodex, noTools: false}
	got := resolveSupervisorRunner(context.Background(), codex, "", agent.SelectConfig{}, secrets.NewMemStore())
	if got != nil {
		t.Fatalf("unsafe production runner became supervisor runner: %T", got)
	}

	claude := supervisorRunnerStub{id: agent.IDClaude, noTools: true}
	got = resolveSupervisorRunner(context.Background(), claude, "", agent.SelectConfig{}, secrets.NewMemStore())
	if got == nil || got.ID() != agent.IDClaude {
		t.Fatalf("safe production runner was not retained: %T", got)
	}
}

func TestPrintBannerFirstRunShowsPassword(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "abcd-efgh-ijkl-mnop", false)
	out := b.String()
	if !strings.Contains(out, "abcd-efgh-ijkl-mnop") {
		t.Error("first-run banner omitted the one-time password")
	}
	if !strings.Contains(out, "http://127.0.0.1:8090") {
		t.Error("banner omitted the listen URL")
	}
}

func TestPrintBannerLaterRunHidesPassword(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "", false)
	if strings.Contains(strings.ToLower(b.String()), "password") {
		t.Errorf("later-run banner mentioned a password: %s", b.String())
	}
}

func TestPrintBannerKeychainFallbackWarning(t *testing.T) {
	var b strings.Builder
	printBanner(&b, "127.0.0.1:8090", "/data", "", true)
	if !strings.Contains(b.String(), "secrets.json") {
		t.Error("fallback banner omitted the secrets.json warning")
	}
}

func TestRunBootsAndServesThenShutsDown(t *testing.T) {
	dir := t.TempDir()
	// Keep the boot hermetic: never let toolfetch reach the network from a test
	// runner without local ffmpeg (auto_download defaults to true).
	cfgYAML := "tools:\n  auto_download: false\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir: dir,
			Listen:  "127.0.0.1:8137",
			Version: "test",
			Out:     io.Discard,
		})
	}()

	// Poll until the server answers.
	base := "http://127.0.0.1:8137"
	if !waitReady(base+"/api/v1/system", 10*time.Second) {
		cancel()
		<-done
		t.Fatal("server never became ready")
	}

	// Unauthed /system must be 401.
	resp, err := http.Get(base + "/api/v1/system")
	if err != nil {
		t.Fatalf("GET /system: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthed /system = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Embedded UI root serves 200.
	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down")
	}
}

func TestEventPersisterDropsOnOverflowAndDrains(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	var buf strings.Builder
	p := newEventPersister(db, &buf, 2)

	// Enqueue past capacity BEFORE the drain starts so the surplus is dropped
	// deterministically (2 fit the buffer, the rest overflow).
	const n = 6
	for i := 0; i < n; i++ {
		p.enqueue(events.Event{
			ID:     uint64(i + 1),
			Type:   "book.state",
			BookID: int64(i + 1),
			Data:   []byte(`{"state":"asr"}`),
		})
	}
	if got := p.dropped.Load(); got != n-2 {
		t.Fatalf("dropped = %d, want %d", got, n-2)
	}
	if !strings.Contains(buf.String(), "dropped") {
		t.Errorf("overflow was not logged: %q", buf.String())
	}

	// Start and close: the 2 buffered events must drain into the durable log.
	p.start()
	p.Close()

	evs, err := db.ListEvents(context.Background(), 0, 0, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("persisted %d events, want 2 (the rest overflowed)", len(evs))
	}
	// The hub id round-trips into the durable log (newest first).
	if evs[0].HubID == 0 {
		t.Errorf("hub_id not persisted: %+v", evs[0])
	}
}

func waitReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // test helper
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
