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

	"github.com/kodestar/audiosilo-meta/pkg/pack"
)

// TestComposeRoundTrip proves the composed bodies parse as `ok` through the real
// upstream metaissue tool, the way each repository's intake bot runs it since the
// 2026-08-21 community-repo split:
//
//   - a characters/recaps sidecar is composed by the COMMUNITY repository's intake
//     (`--profile community`, over a data root holding only works-community), which
//     cannot see the works family and so verifies the sidecar's work slug against the
//     newest data release artifact (`--works-db meta.sqlite`);
//   - an add-work proposal is composed by the CORE repository's intake
//     (`--profile core`).
//
// It is env-gated on AUDIOSILO_META_DIR (the path to a local audiosilo-meta checkout)
// and skips otherwise, so CI without the checkout stays green. It never writes into
// the checkout: metaissue and metabuild are built into a temp dir, and every data
// tree is a throwaway pack-layout seed.
func TestComposeRoundTrip(t *testing.T) {
	metaDir := os.Getenv("AUDIOSILO_META_DIR")
	if metaDir == "" {
		t.Skip("set AUDIOSILO_META_DIR to a local audiosilo-meta checkout to run the round-trip test")
	}
	if _, err := os.Stat(filepath.Join(metaDir, "cmd", "metaissue")); err != nil {
		t.Skipf("AUDIOSILO_META_DIR does not look like a meta checkout: %v", err)
	}
	bin := t.TempDir()
	metaissue := buildMetaTool(t, metaDir, bin, "metaissue")
	metabuild := buildMetaTool(t, metaDir, bin, "metabuild")

	// The newest data release stand-in: a core-only artifact built from the seed, which
	// is what the community intake's --works-db reads.
	coreSeed := seedCoreTree(t)
	worksDB := filepath.Join(t.TempDir(), "meta.sqlite")
	runTool(t, metabuild, "-data", coreSeed, "-o", worksDB)

	charactersPayload := `{"work":"existing-work","characters":[{"id":"alice","name":"Alice","reveal":{"chapter":1},"description":"A brave adventurer introduced early in the book."}],"license":"CC-BY-SA-4.0","sources":[{"type":"community"}]}`
	recapsPayload := `{"work":"existing-work","recaps":[{"through":{"chapter":1},"text":"So far, the opening chapter has set the scene and the adventure is under way."}],"license":"CC-BY-SA-4.0","sources":[{"type":"community"}]}`

	community := func(t *testing.T) []string {
		// An empty community root: the works-community family is created by the write.
		return []string{"--profile", "community", "--data", filepath.Join(t.TempDir(), "data"), "--works-db", worksDB}
	}

	t.Run("characters", func(t *testing.T) {
		_, body, labels := CharactersIssue("existing-work", []byte(charactersPayload), "")
		v := runMetaissue(t, metaissue, labels, body, community(t)...)
		assertFilesUnder(t, v, "works-community/")
	})

	t.Run("recaps", func(t *testing.T) {
		_, body, labels := RecapsIssue("existing-work", []byte(recapsPayload), "")
		v := runMetaissue(t, metaissue, labels, body, community(t)...)
		assertFilesUnder(t, v, "works-community/")
	})

	// Why the release gate exists: a sidecar keyed by a work no data release holds
	// yet (a just-merged add-work) is not composed - the community intake answers
	// needs-human, since the artifact it verifies against may simply be stale.
	t.Run("unreleased work is refused", func(t *testing.T) {
		payload := strings.ReplaceAll(charactersPayload, "existing-work", "not-yet-released")
		_, body, labels := CharactersIssue("not-yet-released", []byte(payload), "")
		v := runMetaissueStatus(t, metaissue, labels, body, community(t)...)
		if v.Status != "needs-human" {
			t.Fatalf("verdict = %q (%v), want needs-human for a slug no release holds", v.Status, v.Messages)
		}
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
		_, body, labels := WorkIssue(p)
		v := runMetaissue(t, metaissue, labels, body, "--profile", "core", "--data", seedCoreTree(t))
		assertFilesUnder(t, v, "works/")
	})
}

// verdict is metaissue's machine-readable result.
type verdict struct {
	Status   string   `json:"status"`
	Messages []string `json:"messages"`
	Files    []string `json:"files"`
}

// buildMetaTool builds one of the checkout's commands into dir (read-only use of the
// checkout: only the Go build cache is written).
func buildMetaTool(t *testing.T, metaDir, dir, name string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/"+name)
	cmd.Dir = metaDir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

// runTool runs a built tool and returns its stdout, failing the test on a non-zero
// exit.
func runTool(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\nstderr: %s\nstdout: %s", filepath.Base(tool), args, err, errBuf.String(), out.String())
	}
	return out.Bytes()
}

// runMetaissue runs metaissue over body with the given routing labels plus extra
// flags (the profile, the data root, the works artifact), asserting verdict "ok".
func runMetaissue(t *testing.T, metaissue string, labels []string, body string, extra ...string) verdict {
	t.Helper()
	v := runMetaissueStatus(t, metaissue, labels, body, extra...)
	if v.Status != "ok" {
		t.Fatalf("verdict status = %q, want ok\nmessages: %v\nfiles: %v", v.Status, v.Messages, v.Files)
	}
	t.Logf("metaissue verdict ok, files: %v", v.Files)
	return v
}

// runMetaissueStatus runs metaissue and returns its verdict, whatever the status.
func runMetaissueStatus(t *testing.T, metaissue string, labels []string, body string, extra ...string) verdict {
	t.Helper()
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--labels", string(labelsJSON), "--body", bodyFile, "--date", "2026-07-17"}, extra...)
	out := runTool(t, metaissue, args...)
	var v verdict
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("decode verdict: %v\nstdout: %s", err, out)
	}
	return v
}

// assertFilesUnder checks the verdict names at least one pack of the expected
// family: a sidecar lands in works-community, an add-work in works.
func assertFilesUnder(t *testing.T, v verdict, family string) {
	t.Helper()
	for _, f := range v.Files {
		if strings.Contains(f, family) {
			return
		}
	}
	t.Fatalf("verdict files %v name no %s pack", v.Files, family)
}

// seedCoreTree writes a small, self-consistent CORE data tree (people + one existing
// work with its recording) in the range-packed layout, through meta's own pkg/pack
// under the core profile - so the add-work import writes into a valid tree and the
// built artifact holds the work a sidecar attaches to.
func seedCoreTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := pack.OpenProfile(dir, pack.ProfileCore)
	if err != nil {
		t.Fatal(err)
	}
	src := `[{"type": "user", "imported_at": "2026-07-01"}]`
	people := map[string]string{
		"jane-doe":   `{"id": "jane-doe", "license": "CC0-1.0", "name": "Jane Doe", "sources": ` + src + `}`,
		"john-smith": `{"id": "john-smith", "license": "CC0-1.0", "name": "John Smith", "sources": ` + src + `}`,
	}
	for slug, entry := range people {
		if err := st.Upsert(pack.FamilyPeople, slug, json.RawMessage(entry)); err != nil {
			t.Fatal(err)
		}
	}
	work := `{
  "authors": ["jane-doe"], "id": "existing-work", "language": "en", "license": "CC0-1.0",
  "sources": ` + src + `, "title": "Existing Work",
  "recordings": {"john-smith-2020": {
    "abridged": false, "asin": [{"asin": "B000000001", "region": "us"}], "id": "john-smith-2020",
    "language": "en", "license": "CC0-1.0", "narrators": ["john-smith"], "runtime_min": 400,
    "sources": ` + src + `, "work": "existing-work"}}
}`
	if err := st.Upsert(pack.FamilyWorks, "existing-work", json.RawMessage(work)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	return dir
}
