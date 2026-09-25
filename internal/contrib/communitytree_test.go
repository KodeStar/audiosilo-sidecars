package contrib

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-meta/pkg/check"
	"github.com/kodestar/audiosilo-meta/pkg/pack"
)

// Schema-valid sidecar bodies (the same payloads the round-trip test proves the real
// intake bot accepts).
func charactersSidecar(work string) []byte {
	return []byte(fmt.Sprintf(`{"work":%q,"characters":[{"id":"alice","name":"Alice","reveal":{"chapter":1},`+
		`"description":"A brave adventurer introduced early in the book."}],"license":"CC-BY-SA-4.0","sources":[{"type":"community"}]}`, work))
}

func recapsSidecar(work string) []byte {
	return []byte(fmt.Sprintf(`{"work":%q,"recaps":[{"through":{"chapter":1},`+
		`"text":"So far, the opening chapter has set the scene and the adventure is under way."}],"license":"CC-BY-SA-4.0","sources":[{"type":"community"}]}`, work))
}

// seedCommunityTree writes a valid works-community tree (the community repository's
// data/ root) holding one characters member per given work slug, through pkg/pack so
// the fixture is placed and rendered exactly as upstream's tooling would.
func seedCommunityTree(t *testing.T, works ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := pack.OpenProfile(dir, pack.ProfileCommunity)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		entry, _ := json.Marshal(map[string]json.RawMessage{"characters": charactersSidecar(w)})
		if err := st.Upsert(pack.FamilyWorksCommunity, w, entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if res := check.LoadProfile(dir, pack.ProfileCommunity); len(res.Problems) > 0 {
		t.Fatalf("seed tree invalid: %v", res.Problems)
	}
	return dir
}

// entryMembers reads a work's entry back from a tree and returns its member names.
func entryMembers(t *testing.T, dir, work string) []string {
	t.Helper()
	st, err := pack.OpenProfile(dir, pack.ProfileCommunity)
	if err != nil {
		t.Fatal(err)
	}
	raw, found, err := st.Get(pack.FamilyWorksCommunity, work)
	if err != nil || !found {
		t.Fatalf("entry %s: found=%v err=%v", work, found, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

func TestEditCommunityTreeNewEntry(t *testing.T) {
	dir := seedCommunityTree(t, "alpha-work")
	edit, err := EditCommunityTree(dir, "beta-work", []SidecarMember{
		{Member: "characters", Content: charactersSidecar("beta-work")},
		{Member: "recaps", Content: recapsSidecar("beta-work")},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !slices.Equal(edit.Applied, []string{"characters", "recaps"}) || len(edit.Refused) != 0 {
		t.Fatalf("applied=%v refused=%v", edit.Applied, edit.Refused)
	}
	if edit.Pack != "data/works-community/0/0.json" || len(edit.Files) != 1 || edit.Files[edit.Pack] == nil {
		t.Fatalf("pack=%q files=%v, want the one pack data/works-community/0/0.json", edit.Pack, keys(edit.Files))
	}
	if got := entryMembers(t, dir, "beta-work"); !slices.Equal(got, []string{"characters", "recaps"}) {
		t.Fatalf("beta-work members = %v", got)
	}
	// The committed bytes are exactly what is on disk: canonical, parseable.
	f, err := pack.Parse(edit.Files[edit.Pack])
	if err != nil {
		t.Fatalf("written pack does not parse: %v", err)
	}
	if !slices.Equal(f.Slugs(), []string{"alpha-work", "beta-work"}) {
		t.Fatalf("pack keys = %v", f.Slugs())
	}
}

// TestEditCommunityTreeKeepsSiblingMember: adding recaps to a work that already has
// characters is a read-modify-write - the characters member survives byte for byte.
func TestEditCommunityTreeKeepsSiblingMember(t *testing.T) {
	dir := seedCommunityTree(t, "alpha-work")
	before := entryRaw(t, dir, "alpha-work", "characters")
	edit, err := EditCommunityTree(dir, "alpha-work", []SidecarMember{{Member: "recaps", Content: recapsSidecar("alpha-work")}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !slices.Equal(edit.Applied, []string{"recaps"}) {
		t.Fatalf("applied = %v", edit.Applied)
	}
	if got := entryMembers(t, dir, "alpha-work"); !slices.Equal(got, []string{"characters", "recaps"}) {
		t.Fatalf("members = %v, want the sibling kept", got)
	}
	if after := entryRaw(t, dir, "alpha-work", "characters"); !bytes.Equal(compact(t, before), compact(t, after)) {
		t.Fatalf("sibling member changed:\n%s\n->\n%s", before, after)
	}
}

// TestEditCommunityTreeRefusesExistingMember: a member the entry already carries is
// never overwritten - replacing a sidecar is the maintainers' call - and when nothing
// applies, nothing is written at all.
func TestEditCommunityTreeRefusesExistingMember(t *testing.T) {
	dir := seedCommunityTree(t, "alpha-work")
	edit, err := EditCommunityTree(dir, "alpha-work", []SidecarMember{
		{Member: "characters", Content: charactersSidecar("alpha-work")},
		{Member: "recaps", Content: recapsSidecar("alpha-work")},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !slices.Equal(edit.Applied, []string{"recaps"}) || !strings.Contains(edit.Refused["characters"], "maintainers") {
		t.Fatalf("applied=%v refused=%v, want recaps placed and characters refused", edit.Applied, edit.Refused)
	}

	only, err := EditCommunityTree(dir, "alpha-work", []SidecarMember{{Member: "characters", Content: charactersSidecar("alpha-work")}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(only.Applied) != 0 || only.Changed() {
		t.Fatalf("an all-refused edit wrote something: %+v", only)
	}
}

// TestEditCommunityTreeDueSplit: a pack at the works-community entry cap (200) splits
// when the edit adds one more - pkg/pack does the placement, and the edit reports
// every pack file it wrote.
func TestEditCommunityTreeDueSplit(t *testing.T) {
	works := make([]string, 200)
	for i := range works {
		works[i] = fmt.Sprintf("work-%03d", i)
	}
	dir := seedCommunityTree(t, works...)
	edit, err := EditCommunityTree(dir, "work-100a", []SidecarMember{{Member: "recaps", Content: recapsSidecar("work-100a")}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(edit.Files) < 2 {
		t.Fatalf("files = %v, want the split's two packs", keys(edit.Files))
	}
	for p := range edit.Files {
		if !strings.HasPrefix(p, "data/works-community/") {
			t.Fatalf("file %q outside the community family", p)
		}
	}
	if _, ok := edit.Files[edit.Pack]; !ok {
		t.Fatalf("entry pack %q not among the written files %v", edit.Pack, keys(edit.Files))
	}
	if res := check.LoadProfile(dir, pack.ProfileCommunity); len(res.Problems) > 0 {
		t.Fatalf("tree invalid after split: %v", res.Problems)
	}
}

// TestEditCommunityTreeRejectsMismatchedWork: the sidecar's own work backref must be
// the entry key; a mismatch is an error, never a silently re-keyed contribution.
func TestEditCommunityTreeRejectsMismatchedWork(t *testing.T) {
	dir := seedCommunityTree(t, "alpha-work")
	if _, err := EditCommunityTree(dir, "beta-work", []SidecarMember{{Member: "recaps", Content: recapsSidecar("gamma-work")}}); err == nil {
		t.Fatal("a sidecar naming another work must be refused")
	}
}

// TestEditCommunityTreeInvalidAfterEdit: an edit that leaves the tree failing meta's
// own check is ErrCommunityTreeInvalid - no PR may be opened from it.
func TestEditCommunityTreeInvalidAfterEdit(t *testing.T) {
	dir := seedCommunityTree(t, "alpha-work")
	bad := []byte(`{"work":"beta-work","characters":[],"license":"CC0-1.0","sources":[{"type":"community"}]}`)
	_, err := EditCommunityTree(dir, "beta-work", []SidecarMember{{Member: "characters", Content: bad}})
	if !errors.Is(err, ErrCommunityTreeInvalid) {
		t.Fatalf("err = %v, want ErrCommunityTreeInvalid", err)
	}
}

// --- tarball extraction ---

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractCommunityData(t *testing.T) {
	arc := tarball(t, map[string]string{
		"owner-repo-abc/README.md":                           "readme",
		"owner-repo-abc/data/works-community/0/0.json":       `{"entries":{}}`,
		"owner-repo-abc/data/works-community/0/m.json":       `{"entries":{}}`,
		"owner-repo-abc/data/works-community/0/notes.txt":    "skip me",
		"owner-repo-abc/scripts/key-check.sh":                "#!/bin/sh",
		"owner-repo-abc/data/works-community-extra/0/0.json": `{}`,
	})
	dir := filepath.Join(t.TempDir(), "data")
	if err := ExtractCommunityData(bytes.NewReader(arc), dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	var got []string
	_ = filepath.WalkDir(filepath.Dir(dir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(got)
	if !slices.Equal(got, []string{"works-community/0/0.json", "works-community/0/m.json"}) {
		t.Fatalf("extracted = %v, want only the works-community packs", got)
	}
}

func TestExtractCommunityDataRefusesTraversal(t *testing.T) {
	arc := tarball(t, map[string]string{
		"owner-repo-abc/data/works-community/../../../evil.json": `{}`,
	})
	if err := ExtractCommunityData(bytes.NewReader(arc), filepath.Join(t.TempDir(), "data")); err == nil {
		t.Fatal("a climbing path must be refused")
	}
}

func entryRaw(t *testing.T, dir, work, member string) []byte {
	t.Helper()
	st, err := pack.OpenProfile(dir, pack.ProfileCommunity)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := st.Get(pack.FamilyWorksCommunity, work)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m[member]
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestEditCommunityTreeRealTree runs the pack edit over a COPY of a real community
// checkout's data/ (env-gated on AUDIOSILO_META_COMMUNITY_DIR, skipped otherwise): a
// new entry is placed and the whole real tree still validates, and a member the tree
// already holds is refused.
func TestEditCommunityTreeRealTree(t *testing.T) {
	src := os.Getenv("AUDIOSILO_META_COMMUNITY_DIR")
	if src == "" {
		t.Skip("set AUDIOSILO_META_COMMUNITY_DIR to a local audiosilo-meta-community checkout to run")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(src, "data"))); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	edit, err := EditCommunityTree(dir, "zz-sidecars-roundtrip-probe", []SidecarMember{
		{Member: "characters", Content: charactersSidecar("zz-sidecars-roundtrip-probe")},
	})
	if err != nil {
		t.Fatalf("edit over the real tree: %v", err)
	}
	t.Logf("real tree: placed in %s, files %v, deleted %v", edit.Pack, keys(edit.Files), edit.Deleted)

	// Any existing entry's member is refused, never overwritten.
	st, err := pack.OpenProfile(dir, pack.ProfileCommunity)
	if err != nil {
		t.Fatal(err)
	}
	var existing string
	for _, ref := range st.Tree(pack.FamilyWorksCommunity).Packs() {
		f, err := st.Pack(ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, slug := range f.Slugs() {
			raw, _ := f.Get(slug)
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) == nil && m["characters"] != nil {
				existing = slug
				break
			}
		}
		if existing != "" {
			break
		}
	}
	if existing == "" {
		t.Skip("the real tree holds no characters member to probe the refusal with")
	}
	again, err := EditCommunityTree(dir, existing, []SidecarMember{{Member: "characters", Content: charactersSidecar(existing)}})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(again.Applied) != 0 || again.Refused["characters"] == "" {
		t.Fatalf("existing member was not refused: %+v", again)
	}
}
