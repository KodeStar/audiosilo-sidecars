package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// seriesPriorFile is the durable record of an earlier series volume's community-published
// recap material, written into the book's work dir the first time it is obtained. The
// scheduler owns the NAME (readmit clears a negative record); the schema below is ours.
const seriesPriorFile = scheduler.SeriesPriorFile

// seriesPriorStagedName is the staged file the synthesis/audit/fix agents read it from.
const seriesPriorStagedName = "series-previously.md"

// seriesPrior is the persisted shape of metaops.SeriesPrior plus the fetch stamp. The
// file is the record of a DETERMINATION as much as of its content: its presence is what
// keeps a book a non-opener across later rounds.
//
// A NEGATIVE determination is recorded too (None), so a book with no upstream prior
// stops re-walking its series on every synthesis / audit / fix / validate entry. Only a
// DEFINITIVE negative is written - an outage maps to an empty result upstream, and
// freezing that would pin opener=true, the exact bug class this record exists to fix.
//
// A negative is NOT permanent: scheduler.readmit clears it (never a positive one), so a
// Retry re-derives against upstream - the case being a human who has since contributed
// the predecessor volume. Deleting series_prior.json by hand does the same.
type seriesPrior struct {
	// None marks a recorded "this book has no upstream prior". present() stays false
	// for it, so the opener verdict is exactly what it would be with no record - only
	// the repeated lookup is skipped.
	None              bool     `json:"none,omitempty"`
	WorkID            string   `json:"work_id,omitempty"`
	Title             string   `json:"title"`
	Authors           []string `json:"authors,omitempty"`
	InShort           string   `json:"in_short,omitempty"`
	Ending            string   `json:"ending,omitempty"`
	FinalRecap        string   `json:"final_recap,omitempty"`
	FinalRecapChapter int      `json:"final_recap_chapter,omitempty"`
	FetchedAt         string   `json:"fetched_at"`
}

// present reports whether a predecessor volume was found.
func (p seriesPrior) present() bool { return strings.TrimSpace(p.WorkID) != "" }

// recorded reports whether this is a usable determination (positive or negative), as
// opposed to a zero value / truncated file that must be re-derived.
func (p seriesPrior) recorded() bool { return p.None || p.present() }

// seriesStatus resolves the two series facts the sidecar stages share: whether this
// book opens its series (an opener must NOT carry a chapter-0 "previously" recap) and,
// when it does not, the predecessor material the recap is grounded in.
//
// The LOCAL predecessor wins: a same-series volume processed on this daemon already
// proves the book is not an opener, and its knowledge sheet is inherited into facts/,
// so nothing needs staging. Only when there is no local predecessor is the community
// database consulted - a volume already covered upstream is never re-derived locally,
// which is exactly how a book-2 came to be treated as its own series opener.
func (e *Executor) seriesStatus(ctx context.Context, book store.Book) (bool, seriesPrior, error) {
	if strings.TrimSpace(book.Series) == "" {
		return true, seriesPrior{}, nil
	}
	_, found, err := findSeriesPredecessor(ctx, e.db, book)
	if err != nil {
		return false, seriesPrior{}, err
	}
	if found {
		return false, seriesPrior{}, nil
	}
	prior, err := e.seriesPriorMaterial(ctx, book)
	if err != nil {
		return false, seriesPrior{}, err
	}
	return !prior.present(), prior, nil
}

// seriesPriorMaterial returns the upstream predecessor's recap material, or a zero
// value when there is none.
//
// A successful lookup is PERSISTED and preferred on every later call. The opener
// verdict is re-derived on each synthesis/audit/fix/validate entry, and a metadata
// outage between rounds must never flip a book from non-opener back to opener: the
// chapter-0 recap already written would become a hard validation error the fixer is
// told to delete while the auditor keeps demanding it back, and the loop can never
// converge. The reverse flip (opener -> non-opener, when upstream gains the earlier
// volume mid-run) is safe: it only asks for a recap that can then be added.
func (e *Executor) seriesPriorMaterial(ctx context.Context, book store.Book) (seriesPrior, error) {
	path := filepath.Join(book.WorkDir, seriesPriorFile)
	prior, ok, err := loadSeriesPrior(path)
	if err != nil {
		return seriesPrior{}, err
	}
	if ok {
		return prior, nil
	}
	if e.meta == nil {
		// Metadata is disabled. Record nothing: enabling it later must be free to
		// derive a real answer.
		return seriesPrior{}, nil
	}
	up, definitive, err := e.meta.SeriesPriorFor(ctx, metaops.SeriesPriorQuery{
		WorkID:     book.WorkID,
		SeriesName: book.Series,
		SeriesPos:  book.SeriesPos,
	})
	if err != nil {
		// metaops degrades an outage to an empty result, so an error here is a
		// cancelled context or a client fault. Neither may park the book: with no
		// prior material it keeps today's behaviour until a later round records some.
		e.log.Warn("series prior lookup failed", "book", book.ID, "err", err)
		return seriesPrior{}, nil
	}
	if up.Empty() {
		if !definitive {
			// Degraded, not settled: leave no record so the next entry asks again.
			return seriesPrior{}, nil
		}
		none := seriesPrior{None: true, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := writeSeriesPrior(path, none); err != nil {
			return seriesPrior{}, fmt.Errorf("record %s: %w", seriesPriorFile, err)
		}
		return none, nil
	}
	prior = seriesPrior{
		WorkID:            up.WorkID,
		Title:             up.Title,
		Authors:           up.Authors,
		InShort:           up.InShort,
		Ending:            up.Ending,
		FinalRecap:        up.FinalRecap,
		FinalRecapChapter: up.FinalRecapChapter,
		FetchedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeSeriesPrior(path, prior); err != nil {
		return seriesPrior{}, fmt.Errorf("record %s: %w", seriesPriorFile, err)
	}
	return prior, nil
}

// loadSeriesPrior reads the persisted record, positive or negative. ok=false means
// there is none. A malformed file is a real error: silently treating it as absent
// would retract the determination it exists to hold.
func loadSeriesPrior(path string) (seriesPrior, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		if os.IsNotExist(err) {
			return seriesPrior{}, false, nil
		}
		return seriesPrior{}, false, err
	}
	var p seriesPrior
	if err := json.Unmarshal(raw, &p); err != nil {
		return seriesPrior{}, false, fmt.Errorf("%s: %w", seriesPriorFile, err)
	}
	if !p.recorded() {
		return seriesPrior{}, false, nil
	}
	return p, true, nil
}

// writeSeriesPrior persists the record atomically.
func writeSeriesPrior(path string, p seriesPrior) error {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(out, '\n'), 0o644)
}

// markdown renders the staged series-previously.md: a header naming the volume the
// material came from and the licence of the text, then the material itself. It states
// the ONE permitted use so the rule travels with the file, not only with the prompt.
// It never claims the volume is the ADJACENT one - metaops walks past nearer volumes
// that publish no recaps, so asserting adjacency would invite the agent to present an
// older book's events as everything that happened since.
func (p seriesPrior) markdown() string {
	var b strings.Builder
	b.WriteString("# Previously in this series\n\n")
	by := strings.Join(p.Authors, ", ")
	if by == "" {
		by = "Unknown"
	}
	fmt.Fprintf(&b, "Source: the community metadata database at meta.audiosilo.app, for %q by %s -\n"+
		"an earlier volume in this series. This text is CC BY-SA 3.0 community writing\n"+
		"about that book. It is NOT the novel and NOT this book's source text.\n\n", p.Title, by)
	b.WriteString("Use it ONLY to ground this book's `chapter: 0` `scope: \"series\"` \"previously\"\n" +
		"recap. Rewrite it in your own words - never copy a sentence from it - and claim\n" +
		"only what it states. It is the material for the one volume named above, not a\n" +
		"summary of everything that happened before this book, so do not present it as\n" +
		"the whole story so far.\n")
	if p.InShort != "" {
		b.WriteString("\n## The earlier book in short\n\n" + p.InShort + "\n")
	}
	if p.Ending != "" {
		b.WriteString("\n## Where the earlier book left off\n\n" + p.Ending + "\n")
	}
	if p.FinalRecap != "" {
		fmt.Fprintf(&b, "\n## Its final recap (through chapter %d)\n\n%s\n", p.FinalRecapChapter, p.FinalRecap)
	}
	return b.String()
}

// stageSeriesPrior writes the predecessor material into a staged dir. A book with no
// prior material stages nothing, and its prompt does not mention the file.
func stageSeriesPrior(st *agent.Staging, p seriesPrior) error {
	if !p.present() {
		return nil
	}
	if err := st.WriteFile(seriesPriorStagedName, []byte(p.markdown())); err != nil {
		return fmt.Errorf("stage %s: %w", seriesPriorStagedName, err)
	}
	return nil
}
