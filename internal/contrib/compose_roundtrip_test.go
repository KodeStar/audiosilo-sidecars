package contrib

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestComposeRoundTrip proves the composed bodies parse as `ok` through the real
// upstream metaissue tool. It is env-gated on AUDIOSILO_META_DIR (the path to a
// local audiosilo-meta checkout) and skips otherwise, so CI without the checkout
// stays green. It never writes into the checkout: it builds a throwaway minimal
// data tree in a temp dir and runs `go run ./cmd/metaissue` against THAT with the
// checkout only as the toolchain's working directory.
func TestComposeRoundTrip(t *testing.T) {
	metaDir := os.Getenv("AUDIOSILO_META_DIR")
	if metaDir == "" {
		t.Skip("set AUDIOSILO_META_DIR to a local audiosilo-meta checkout to run the round-trip test")
	}
	if _, err := os.Stat(filepath.Join(metaDir, "cmd", "metaissue")); err != nil {
		t.Skipf("AUDIOSILO_META_DIR does not look like a meta checkout: %v", err)
	}

	charactersPayload := `{"work":"existing-work","characters":[{"id":"alice","name":"Alice","reveal":{"chapter":1},"description":"A brave adventurer introduced early in the book."}],"license":"CC-BY-SA-3.0","sources":[{"type":"community"}]}`
	recapsPayload := `{"work":"existing-work","recaps":[{"through":{"chapter":1},"text":"So far, the opening chapter has set the scene and the adventure is under way."}],"license":"CC-BY-SA-3.0","sources":[{"type":"community"}]}`

	t.Run("characters", func(t *testing.T) {
		_, body, _ := CharactersIssue("existing-work", []byte(charactersPayload), "")
		runMetaissue(t, metaDir, "data,data:characters", body)
	})

	t.Run("recaps", func(t *testing.T) {
		_, body, _ := RecapsIssue("existing-work", []byte(recapsPayload), "")
		runMetaissue(t, metaDir, "data,data:recaps", body)
	})

	t.Run("work", func(t *testing.T) {
		p := CoreProposal{
			Title:      "Brand New Roundtrip Book",
			Authors:    []string{"Alice Author"},
			Language:   "en-GB",
			Narrators:  []string{"Bob Reader"},
			Abridged:   "Unabridged",
			RuntimeMin: 498,
			ASINs:      []RegionASIN{{Region: "US", ASIN: "B0RT000001"}},
			Sources:    "Audible US product page (read 2026-07-17)",
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("proposal should validate: %v", err)
		}
		_, body, _ := WorkIssue(p)
		runMetaissue(t, metaDir, "data,data:add-work", body)
	})
}

// runMetaissue writes body to a temp file, builds a fresh minimal data tree, and
// runs `go run ./cmd/metaissue` with the given routing labels, asserting the JSON
// verdict status is "ok".
func runMetaissue(t *testing.T, metaDir, labels, body string) {
	t.Helper()
	dataDir := seedMinimalTree(t)

	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/metaissue",
		"--labels", "["+labelsJSON(labels)+"]",
		"--body", bodyFile,
		"--data", dataDir,
		"--date", "2026-07-17",
	)
	cmd.Dir = metaDir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("metaissue run failed: %v\nstderr: %s\nstdout: %s", err, errBuf.String(), out.String())
	}

	var verdict struct {
		Status   string   `json:"status"`
		Messages []string `json:"messages"`
		Files    []string `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &verdict); err != nil {
		t.Fatalf("decode verdict: %v\nstdout: %s", err, out.String())
	}
	if verdict.Status != "ok" {
		t.Fatalf("verdict status = %q, want ok\nmessages: %v\nfiles: %v", verdict.Status, verdict.Messages, verdict.Files)
	}
	t.Logf("metaissue verdict ok, files: %v", verdict.Files)
}

// labelsJSON renders a comma-separated label list as JSON array elements
// ("a,b" -> `"a","b"`) for the --labels flag.
func labelsJSON(labels string) string {
	parts := strings.Split(labels, ",")
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = `"` + p + `"`
	}
	return strings.Join(quoted, ",")
}

// seedMinimalTree writes a small, self-consistent data tree (people + one
// existing work + recording + series) to a temp dir, so the sidecar attaches to
// a real work and the add-work import writes into a valid tree. It mirrors the
// meta repo's own issueform test seed.
func seedMinimalTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"people/ja/jane-doe.json": `{
  "id": "jane-doe",
  "license": "CC0-1.0",
  "name": "Jane Doe",
  "sources": [{"type": "user", "imported_at": "2026-07-01"}]
}`,
		"people/jo/john-smith.json": `{
  "id": "john-smith",
  "license": "CC0-1.0",
  "name": "John Smith",
  "sources": [{"type": "user", "imported_at": "2026-07-01"}]
}`,
		"works/ex/existing-work/work.json": `{
  "authors": ["jane-doe"],
  "id": "existing-work",
  "language": "en",
  "license": "CC0-1.0",
  "sources": [{"type": "user", "imported_at": "2026-07-01"}],
  "title": "Existing Work"
}`,
		"works/ex/existing-work/recordings/john-smith-2020.json": `{
  "abridged": false,
  "asin": [{"asin": "B000000001", "region": "us"}],
  "id": "john-smith-2020",
  "language": "en",
  "license": "CC0-1.0",
  "narrators": ["john-smith"],
  "runtime_min": 400,
  "sources": [{"type": "user", "imported_at": "2026-07-01"}],
  "work": "existing-work"
}`,
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}
