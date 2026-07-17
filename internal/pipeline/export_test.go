package pipeline

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func TestExportSlug(t *testing.T) {
	cases := []struct {
		book store.Book
		want string
	}{
		{store.Book{WorkID: "a-deadly-education", Title: "X"}, "a-deadly-education"},
		{store.Book{WorkID: "Not A Slug", Title: "The Blade Itself"}, "the-blade-itself"},
		{store.Book{Title: "The Blade Itself"}, "the-blade-itself"},
		{store.Book{Title: "!!!"}, "book"},
	}
	for _, c := range cases {
		if got := ExportSlug(c.book); got != c.want {
			t.Errorf("ExportSlug(%+v) = %q, want %q", c.book, got, c.want)
		}
	}
}

func TestExportArchive(t *testing.T) {
	dir := t.TempDir()
	sc := filepath.Join(dir, sidecarsDir)
	if err := os.MkdirAll(sc, 0o755); err != nil {
		t.Fatal(err)
	}

	// No sidecars -> ErrNoSidecars.
	if _, err := ExportArchive(dir, "my-work"); !errors.Is(err, ErrNoSidecars) {
		t.Fatalf("empty = %v, want ErrNoSidecars", err)
	}

	// One sidecar -> one entry.
	if err := os.WriteFile(filepath.Join(sc, charactersFileName), []byte(`{"work":"my-work"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := ExportArchive(dir, "my-work")
	if err != nil {
		t.Fatalf("ExportArchive: %v", err)
	}
	names := zipNames(t, data)
	if len(names) != 1 || names[0] != "works/my/my-work/characters.json" {
		t.Fatalf("entries = %v", names)
	}

	// Both sidecars -> both entries in the meta layout.
	if err := os.WriteFile(filepath.Join(sc, recapsFileName), []byte(`{"work":"my-work"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err = ExportArchive(dir, "my-work")
	if err != nil {
		t.Fatalf("ExportArchive: %v", err)
	}
	names = zipNames(t, data)
	want := []string{"works/my/my-work/characters.json", "works/my/my-work/recaps.json"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("entries = %v, want %v", names, want)
	}
}

func TestCoreProposalJSON(t *testing.T) {
	dir := t.TempDir()
	if _, err := CoreProposalJSON(dir); !errors.Is(err, ErrNoCoreProposal) {
		t.Fatalf("absent = %v, want ErrNoCoreProposal", err)
	}
	cd := filepath.Join(dir, contribDir)
	if err := os.MkdirAll(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cd, coreProposalName), []byte(`{"title":"T"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := CoreProposalJSON(dir)
	if err != nil {
		t.Fatalf("CoreProposalJSON: %v", err)
	}
	if string(raw) != `{"title":"T"}` {
		t.Fatalf("raw = %s", raw)
	}
}

func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}
