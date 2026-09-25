package pipeline

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// ErrNoCoreProposal is returned by CoreProposalJSON when a book's work dir holds no
// prefilled core proposal (the book never needed a core add-work). The api maps it
// to a 404 (via a translated sentinel wired in server.go, since api must not import
// pipeline).
var ErrNoCoreProposal = errors.New("no core proposal in work dir")

// CoreProposalJSON reads a book's prefilled contrib/core_proposal.json (written by
// the contributing stage when the work does not exist upstream) and returns it
// verbatim - it is already CoreProposal-shaped JSON, so the api serves the bytes
// unchanged for the UI to prefill the work-proposal form. ErrNoCoreProposal signals
// the file is absent (a 404). The api injects this as its CoreProposalLoader.
func CoreProposalJSON(workDir string) (json.RawMessage, error) {
	path := filepath.Join(workDir, contribDir, coreProposalName)
	if !fsutil.IsFile(path) {
		return nil, ErrNoCoreProposal
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("core proposal is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// ExportSlug is the slug a book's sidecars ship under: the shared workSlug derivation
// (matched work id when a valid meta slug, else a title-derived placeholder). It is the
// exported seam so the api never duplicates slug logic; the single implementation lives
// in workSlug (sidecars.go), which applies model.ValidSlug uniformly.
func ExportSlug(b store.Book) string {
	return workSlug(b)
}

// ExportArchive zips a book's sidecars for the "keep local" download as
// <slug>/characters.json and/or <slug>/recaps.json - the bare files an intake-issue
// attachment takes. ErrNoSidecars when neither exists; the file set is fixed and the
// slug validated, so no traversal is possible.
func ExportArchive(workDir, slug string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	added := 0
	for _, name := range []string{charactersFileName, recapsFileName} {
		src := filepath.Join(workDir, sidecarsDir, name)
		if !fsutil.IsFile(src) {
			continue
		}
		content, err := os.ReadFile(src) //nolint:gosec // fixed file set under the book's work dir
		if err != nil {
			return nil, err
		}
		w, err := zw.Create(slug + "/" + name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
		added++
	}
	if added == 0 {
		_ = zw.Close()
		return nil, ErrNoSidecars
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
