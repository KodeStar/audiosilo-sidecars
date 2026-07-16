package metaops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// capturePersist records the last StoredOverride written and echoes it back with
// a fixed updated_at, standing in for the store.
type capturePersist struct {
	last  StoredOverride
	calls int
	err   error
}

func (p *capturePersist) fn(_ context.Context, ov StoredOverride) (StoredOverride, error) {
	p.calls++
	if p.err != nil {
		return StoredOverride{}, p.err
	}
	p.last = ov
	ov.UpdatedAt = "2026-01-01T00:00:00Z"
	return ov, nil
}

// newOverrideEnv wires a service over the given fake meta client, a real (idle)
// scan manager, and a capturing persist func, all rooted at a temp allow-list. A
// "book" subdir is created so the allow-list check resolves it on symlinked temp
// dirs (macOS resolves /var -> /private/var only for existing paths).
func newOverrideEnv(t *testing.T, client *Client) (*OverrideService, *capturePersist, []string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "book"), 0o755); err != nil {
		t.Fatal(err)
	}
	scans := NewScanManager(context.Background(), client, "", nil)
	p := &capturePersist{}
	return NewOverrideService(client, scans, p.fn), p, []string{root}
}

func TestOverrideServiceHideOnly(t *testing.T) {
	svc, p, roots := newOverrideEnv(t, NewClient(""))
	sp := filepath.Join(roots[0], "book")

	res, err := svc.Upsert(context.Background(), OverrideRequest{SourcePath: sp, Hidden: true}, roots)
	if err != nil {
		t.Fatalf("Upsert(hide): %v", err)
	}
	if res.Coverage != nil {
		t.Errorf("hide-only should have nil coverage: %+v", res.Coverage)
	}
	if !res.Override.Hidden || res.Override.UpdatedAt == "" {
		t.Errorf("stored row = %+v, want hidden with updated_at", res.Override)
	}
	if p.calls != 1 || !p.last.Hidden || p.last.WorkID != "" {
		t.Errorf("persist got %+v (calls=%d)", p.last, p.calls)
	}
}

func TestOverrideServiceManualMatch(t *testing.T) {
	s := &metaServer{work: map[string]workRow{"w-good": {title: "Good Work", c: true}}}
	client, _ := newMeta(t, s)
	svc, p, roots := newOverrideEnv(t, client)
	sp := filepath.Join(roots[0], "book")

	res, err := svc.Upsert(context.Background(),
		OverrideRequest{SourcePath: sp, WorkID: "w-good"}, roots)
	if err != nil {
		t.Fatalf("Upsert(match): %v", err)
	}
	if res.Coverage == nil || res.Coverage.MatchedBy != "manual" || res.Coverage.WorkID != "w-good" {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	// The resolved title rides onto the persisted row.
	if p.last.WorkID != "w-good" || p.last.WorkTitle != "Good Work" {
		t.Fatalf("persist got %+v", p.last)
	}
	// The scan overlay reflects the match (a manual-match patch is registered under
	// the canonical source path - Upsert canonicalizes before ApplyOverride).
	canonSP, _ := resolvePath(sp)
	svc.scans.mu.Lock()
	patch, ok := svc.scans.patches[canonSP]
	svc.scans.mu.Unlock()
	if !ok || patch.coverage == nil {
		t.Errorf("ApplyOverride not reflected on the scan manager: %+v", patch)
	}
}

func TestOverrideServiceErrorTaxonomy(t *testing.T) {
	s := &metaServer{work: map[string]workRow{"w-good": {title: "Good Work"}}}
	client, _ := newMeta(t, s)
	svc, p, roots := newOverrideEnv(t, client)
	inside := filepath.Join(roots[0], "book")
	ctx := context.Background()

	// Empty source_path -> ErrNoSourcePath (nothing persisted).
	if _, err := svc.Upsert(ctx, OverrideRequest{SourcePath: "  "}, roots); !errors.Is(err, ErrNoSourcePath) {
		t.Errorf("empty source_path = %v, want ErrNoSourcePath", err)
	}
	// Outside the roots -> ErrPathNotAllowed.
	if _, err := svc.Upsert(ctx, OverrideRequest{SourcePath: "/etc/passwd", Hidden: true}, roots); !errors.Is(err, ErrPathNotAllowed) {
		t.Errorf("outside-root = %v, want ErrPathNotAllowed", err)
	}
	// A stale work id -> ErrWorkNotFound.
	if _, err := svc.Upsert(ctx, OverrideRequest{SourcePath: inside, WorkID: "w-missing"}, roots); !errors.Is(err, ErrWorkNotFound) {
		t.Errorf("stale work id = %v, want ErrWorkNotFound", err)
	}
	if p.calls != 0 {
		t.Errorf("no override should have persisted on the error paths, got %d persists", p.calls)
	}

	// A work id with metadata disabled -> ErrDisabled.
	offSvc, _, offRoots := newOverrideEnv(t, NewClient(""))
	offInside := filepath.Join(offRoots[0], "book")
	if _, err := offSvc.Upsert(ctx, OverrideRequest{SourcePath: offInside, WorkID: "w-good"}, offRoots); !errors.Is(err, ErrDisabled) {
		t.Errorf("work id with metadata off = %v, want ErrDisabled", err)
	}

	// An unreachable upstream -> ErrUpstream (distinct from a persist failure).
	deadSvc, _, deadRoots := newOverrideEnv(t, NewClient("http://127.0.0.1:0"))
	deadInside := filepath.Join(deadRoots[0], "book")
	if _, err := deadSvc.Upsert(ctx, OverrideRequest{SourcePath: deadInside, WorkID: "w-good"}, deadRoots); !errors.Is(err, ErrUpstream) {
		t.Errorf("unreachable upstream = %v, want ErrUpstream", err)
	}
}

// TestOverrideServiceCanonicalizesSourcePath proves a trailing-slash (or otherwise
// non-canonical) source_path is stored in the same canonical form a scan computes
// for the book, so the persisted override actually applies to the scanned book.
func TestOverrideServiceCanonicalizesSourcePath(t *testing.T) {
	svc, p, roots := newOverrideEnv(t, NewClient(""))
	book := filepath.Join(roots[0], "book")

	// Upsert using a trailing-slash spelling of the folder.
	if _, err := svc.Upsert(context.Background(),
		OverrideRequest{SourcePath: book + string(filepath.Separator), Hidden: true}, roots); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// The persisted key is the canonical form (symlink-eval'd, trailing slash
	// dropped) a scan's SourcePath also carries.
	want, _ := resolvePath(book)
	if p.last.SourcePath != want {
		t.Fatalf("stored source_path = %q, want canonical %q", p.last.SourcePath, want)
	}
}

func TestOverrideServicePersistFailure(t *testing.T) {
	svc, p, roots := newOverrideEnv(t, NewClient(""))
	p.err = errors.New("disk full")
	sp := filepath.Join(roots[0], "book")

	_, err := svc.Upsert(context.Background(), OverrideRequest{SourcePath: sp, Hidden: true}, roots)
	// A storage failure surfaces as a raw (non-sentinel) error the handler maps
	// to 500, NOT ErrUpstream.
	if err == nil || errors.Is(err, ErrUpstream) {
		t.Fatalf("persist failure = %v, want a raw non-ErrUpstream error", err)
	}
}
