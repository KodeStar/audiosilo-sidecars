package contrib

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// JoinNotes joins the non-empty parts of a contribution row note with "; " (a row
// note is free text that rides to the UI). Shared by the poller and the
// contributing stage so every note is spelled one way.
func JoinNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

// truncateRunes cuts s to at most limit bytes at a rune boundary, marking the cut.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// AdoptLiveWork returns the slug a book's sidecars attach to: live - the slug the
// published catalogue answered under, which for a slug a merge retired is its
// survivor - recorded on the book when it differs from recorded. An empty live
// keeps recorded. db may be nil (a stage test without a store).
func AdoptLiveWork(ctx context.Context, db *store.DB, bookID int64, recorded, live string) (string, error) {
	if live == "" || live == recorded {
		return recorded, nil
	}
	if db != nil {
		if err := db.SetBookWorkID(context.WithoutCancel(ctx), bookID, live); err != nil {
			return recorded, err
		}
	}
	return live, nil
}
