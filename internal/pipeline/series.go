package pipeline

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// findSeriesPredecessor returns the same-series book with the highest series position
// strictly below book's whose work dir already holds facts/knowledge-final.md - the
// carryover seed the spelling and fact-pass stages inherit (the previous volume's
// verified ledger and whole-book knowledge sheet). It returns (nil, false, nil) when
// the book has no series, the store is nil, or no earlier volume qualifies.
//
// Position is parsed as a leading number via state.ParseSeriesPos, so "2.5" sorts
// between 2 and 3 and an unparseable position sorts last (and therefore never qualifies
// as a predecessor). Ties on position break toward the higher id.
//
// The signature is a cross-stage contract (the sidecar stages call it too); do not
// rename it.
func findSeriesPredecessor(ctx context.Context, db *store.DB, book store.Book) (*store.Book, bool, error) {
	if db == nil || strings.TrimSpace(book.Series) == "" {
		return nil, false, nil
	}
	books, err := db.ListBooks(ctx)
	if err != nil {
		return nil, false, err
	}
	target := state.ParseSeriesPos(book.SeriesPos)
	var best *store.Book
	var bestPos float64
	for i := range books {
		cand := books[i]
		if cand.ID == book.ID || cand.Series != book.Series {
			continue
		}
		pos := state.ParseSeriesPos(cand.SeriesPos)
		if pos >= target {
			continue
		}
		if !fsutil.IsFile(filepath.Join(cand.WorkDir, spelling.FactsDir, knowledgeFinalName)) {
			continue
		}
		if best == nil || pos > bestPos || (pos == bestPos && cand.ID > best.ID) {
			b := cand
			best = &b
			bestPos = pos
		}
	}
	if best == nil {
		return nil, false, nil
	}
	return best, true, nil
}
