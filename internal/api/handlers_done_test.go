package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

const doneTestCharacters = `{
  "work": "the-blade-itself",
  "characters": [
    {"id": "logen-ninefingers", "name": "Logen Ninefingers", "role": "protagonist", "reveal": {"chapter": 1}, "description": "A feared Northman warrior."}
  ],
  "license": "CC-BY-SA-3.0",
  "sources": [{"type": "community"}]
}`

const doneTestRecaps = `{
  "work": "the-blade-itself",
  "recaps": [
    {"through": {"chapter": 1}, "text": "Logen escapes the Shanka."}
  ],
  "in_short": "The first book of the First Law trilogy.",
  "ending": "The pieces are set for war.",
  "license": "CC-BY-SA-3.0",
  "sources": [{"type": "community"}]
}`

// seedBook inserts a book with the given work dir and returns its id.
func seedBook(t *testing.T, env *pipelineEnv, workDir string) int64 {
	t.Helper()
	b, err := env.db.CreateBook(context.Background(), store.NewBook{
		SourcePath: filepath.Join(workDir, "src"),
		WorkDir:    workDir,
		Title:      "The Blade Itself",
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	return b.ID
}

// writeWorkSidecar writes body to <workDir>/sidecars/<name>.
func writeWorkSidecar(t *testing.T, workDir, name, body string) {
	t.Helper()
	dir := filepath.Join(workDir, "sidecars")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestBookSidecarsAllowedAndDenied(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	work := t.TempDir()
	writeWorkSidecar(t, work, "characters.json", doneTestCharacters)
	writeWorkSidecar(t, work, "recaps.json", doneTestRecaps)
	id := seedBook(t, env, work)

	// Denied: no token -> 401.
	resp := env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/sidecars", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Allowed: 200 with the flattened metaserve shape.
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/sidecars", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var preview struct {
		Work         string            `json:"work"`
		Characters   []json.RawMessage `json:"characters"`
		Recaps       []json.RawMessage `json:"recaps"`
		RecapSummary *struct {
			InShort string `json:"in_short"`
			Ending  string `json:"ending"`
		} `json:"recap_summary"`
		License json.RawMessage `json:"license"`
		Sources json.RawMessage `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if preview.Work != "the-blade-itself" {
		t.Errorf("work = %q", preview.Work)
	}
	if len(preview.Characters) != 1 {
		t.Errorf("characters len = %d, want 1", len(preview.Characters))
	}
	if len(preview.Recaps) != 1 {
		t.Errorf("recaps len = %d, want 1", len(preview.Recaps))
	}
	if preview.RecapSummary == nil || preview.RecapSummary.InShort == "" || preview.RecapSummary.Ending == "" {
		t.Errorf("recap_summary = %+v, want flattened in_short/ending", preview.RecapSummary)
	}
	// The file-level wrappers must be dropped.
	if preview.License != nil || preview.Sources != nil {
		t.Errorf("preview should not carry license/sources: license=%s sources=%s", preview.License, preview.Sources)
	}
}

func TestBookSidecarsMissingFiles404(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	// Book exists but its work dir has no sidecar files -> 404.
	id := seedBook(t, env, t.TempDir())
	resp := env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/sidecars", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing-files status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBookSidecarsUnknownBook404(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	resp := env.do(t, http.MethodGet, "/api/v1/books/99999/sidecars", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown-book status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBookEventsAllowedAndDenied(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)
	ctx := context.Background()

	id := seedBook(t, env, t.TempDir())
	// Seed three events with increasing hub ids; ListEvents returns newest first (by id).
	for i, typ := range []string{"stage.progress", "book.state", "stage.progress"} {
		payload := json.RawMessage(`{"n":` + i64(int64(i)) + `}`)
		if err := env.db.InsertEvent(ctx, time.Now(), uint64(i+1), typ, id, payload); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	// Denied: no token -> 401.
	resp := env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/events", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Allowed: newest first, correct shape.
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/events", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got bookEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(got.Events) != 3 {
		t.Fatalf("events len = %d, want 3", len(got.Events))
	}
	// Newest first: the last-inserted row has the highest id.
	if got.Events[0].ID <= got.Events[2].ID {
		t.Errorf("events not newest-first: ids %d, %d, %d", got.Events[0].ID, got.Events[1].ID, got.Events[2].ID)
	}
	if got.Events[0].Type == "" || got.Events[0].TS == "" {
		t.Errorf("event missing type/ts: %+v", got.Events[0])
	}
	if string(got.Events[0].Payload) == "" {
		t.Errorf("event payload not passed through: %+v", got.Events[0])
	}

	// limit clamps: limit=1 returns exactly one row.
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/events?limit=1", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("limit=1 status = %d, want 200", resp.StatusCode)
	}
	var one bookEventsResponse
	_ = json.NewDecoder(resp.Body).Decode(&one)
	resp.Body.Close()
	if len(one.Events) != 1 {
		t.Errorf("limit=1 events len = %d, want 1", len(one.Events))
	}

	// A non-numeric limit falls back to the default (all 3 fit under it).
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/events?limit=abc", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bad-limit status = %d, want 200", resp.StatusCode)
	}
	var deflt bookEventsResponse
	_ = json.NewDecoder(resp.Body).Decode(&deflt)
	resp.Body.Close()
	if len(deflt.Events) != 3 {
		t.Errorf("bad-limit events len = %d, want 3 (default window)", len(deflt.Events))
	}

	// before_id cursor: only events older than the newest id are returned.
	newestID := got.Events[0].ID
	resp = env.do(t, http.MethodGet, "/api/v1/books/"+i64(id)+"/events?before_id="+i64(newestID), token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("before_id status = %d, want 200", resp.StatusCode)
	}
	var older bookEventsResponse
	_ = json.NewDecoder(resp.Body).Decode(&older)
	resp.Body.Close()
	if len(older.Events) != 2 {
		t.Fatalf("before_id events len = %d, want 2 (older than newest)", len(older.Events))
	}
	for _, e := range older.Events {
		if e.ID >= newestID {
			t.Errorf("before_id returned id %d >= cursor %d", e.ID, newestID)
		}
	}
}

func TestBookEventsUnknownBook404(t *testing.T) {
	env := newPipelineEnv(t, nil)
	token := env.login(t)

	resp := env.do(t, http.MethodGet, "/api/v1/books/99999/events", token, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown-book status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// i64 renders an int64 as a decimal string.
func i64(n int64) string { return strconv.FormatInt(n, 10) }
