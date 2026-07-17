package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kodestar/audiosilo-meta/pkg/model"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
)

// ErrNoSidecars is returned by SidecarsView when a book's work dir holds neither
// characters.json nor recaps.json - there is nothing to preview yet. The api maps
// it to a 404 (via a translated sentinel wired in server.go, since api must not
// import pipeline).
var ErrNoSidecars = errors.New("no sidecars in work dir")

// SidecarPreview is the metaserve-API-shaped preview of a book's contributed
// sidecars: the two CC BY-SA files (characters.json + recaps.json) merged into the
// shape the meta.audiosilo.app work API returns, so the web Done panel's vendored
// expressive renderer consumes it unchanged. The file-level license/sources
// wrappers are dropped and the recaps file's in_short/ending are flattened into
// recap_summary. Every sidecar key is omitempty (a book mid-pipeline may have
// produced only one file).
type SidecarPreview struct {
	Work         string            `json:"work,omitempty"`
	Characters   []model.Character `json:"characters,omitempty"`
	Recaps       []model.Recap     `json:"recaps,omitempty"`
	RecapSummary *RecapSummary     `json:"recap_summary,omitempty"`
}

// RecapSummary is the flattened whole-book summary pair from the recaps sidecar
// (both fields optional; the object is omitted entirely when neither is set).
type RecapSummary struct {
	InShort string `json:"in_short,omitempty"`
	Ending  string `json:"ending,omitempty"`
}

// SidecarsView loads a book's characters/recaps sidecars from its work dir and
// composes the preview. It reads the two files independently (reusing the
// sidecarsDir/*FileName constants and the strict decodeSidecarFile guard) rather
// than via loadWorkSidecars, which requires both. It returns ErrNoSidecars when
// neither file exists; a malformed file that DOES exist is a hard error (not a
// 404). The api injects this as its SidecarLoader.
func SidecarsView(workDir string) (SidecarPreview, error) {
	charsPath := filepath.Join(workDir, sidecarsDir, charactersFileName)
	recapsPath := filepath.Join(workDir, sidecarsDir, recapsFileName)

	charsExists := fsutil.IsFile(charsPath)
	recapsExists := fsutil.IsFile(recapsPath)
	if !charsExists && !recapsExists {
		return SidecarPreview{}, ErrNoSidecars
	}

	var view SidecarPreview
	if charsExists {
		var chars model.Characters
		if err := decodeSidecarFile(charsPath, &chars); err != nil {
			return SidecarPreview{}, fmt.Errorf("characters.json: %w", err)
		}
		view.Work = chars.Work
		view.Characters = chars.Characters
	}
	if recapsExists {
		var recs model.Recaps
		if err := decodeSidecarFile(recapsPath, &recs); err != nil {
			return SidecarPreview{}, fmt.Errorf("recaps.json: %w", err)
		}
		if view.Work == "" {
			view.Work = recs.Work
		}
		view.Recaps = recs.Recaps
		if recs.InShort != "" || recs.Ending != "" {
			view.RecapSummary = &RecapSummary{InShort: recs.InShort, Ending: recs.Ending}
		}
	}
	return view, nil
}

// SidecarsViewJSON is the api-facing adapter: it composes the preview and marshals
// it to a JSON document, so the api can serve the bytes without importing pipeline
// types. ErrNoSidecars passes through unchanged for the caller's 404 mapping.
func SidecarsViewJSON(workDir string) (json.RawMessage, error) {
	view, err := SidecarsView(workDir)
	if err != nil {
		return nil, err
	}
	return json.Marshal(view)
}
