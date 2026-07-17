package contrib

import (
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// defaultPollInterval is the fallback poll cadence when the configured interval is
// non-positive.
const defaultPollInterval = 10 * time.Minute

// rowTarget is the lifecycle state a poll tick wants to write onto a contribution
// row: the new status plus the discovered intake-PR pointer (issue mode).
type rowTarget struct {
	status   string
	prNumber int
	prURL    string
}

// RunPoller polls the upstream repo for open-contribution and core-pending state
// changes until ctx is cancelled. The interval is jittered per tick to avoid a
// thundering herd of daemons hitting GitHub in lockstep. Poll failures are logged
// and swallowed; the loop never crashes.
func (s *Service) RunPoller(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timer := time.NewTimer(jitter(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.Poll(ctx)
			timer.Reset(jitter(interval))
		}
	}
}

// jitter returns interval extended by a random 0..interval/4, so staggered daemons
// do not poll GitHub in lockstep.
func jitter(interval time.Duration) time.Duration {
	span := int64(interval) / 4
	if span <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(span+1)) //nolint:gosec // jitter, not security
}

// Poll performs one poll tick: it advances every open contribution row's lifecycle
// against GitHub, then resolves the work slug for any book whose core add-work PR
// has merged (re-admitting it). It works tokenless (public reads); a resolved
// credential is used when present. A failed lookup logs and is skipped so one bad
// row never stalls the rest.
func (s *Service) Poll(ctx context.Context) {
	if s.deps.DB == nil {
		return
	}

	rows, err := s.deps.DB.ListOpenContributions(ctx)
	if err != nil {
		s.logf("contrib poller: list open contributions: %v", err)
		return
	}

	// Resolve the GitHub client lazily and at most once per tick. s.client can shell
	// out to `gh auth token` (up to 15s), so an idle tick with no open rows and no
	// core-pending work to process must not pay that cost. The memoized getter is
	// shared by the open-row loop and resolveCorePending.
	var (
		cli      *Client
		resolved bool
	)
	client := func() *Client {
		if !resolved {
			cli, _ = s.client(ctx)
			resolved = true
		}
		return cli
	}

	if len(rows) > 0 {
		c := client()
		for _, row := range rows {
			if err := s.advanceRow(ctx, c, row); err != nil {
				s.logf("contrib poller: advance book %d %s: %v", row.BookID, row.Kind, err)
			}
		}
	}

	s.resolveCorePending(ctx, client)
}

// advanceRow computes a row's new lifecycle state from GitHub and persists it only
// when something changed (deduped), publishing a contrib.update on a real change.
func (s *Service) advanceRow(ctx context.Context, cli *Client, row store.Contribution) error {
	var target rowTarget
	var err error
	if row.Mode == store.ContribModePR {
		target, err = s.pollPRMode(ctx, cli, row)
	} else {
		target, err = s.pollIssueMode(ctx, cli, row)
	}
	if err != nil {
		return err
	}
	if target.status == row.Status && target.prNumber == row.PRNumber && target.prURL == row.PRURL {
		return nil // steady state: no persist, no publish
	}
	if err := s.deps.DB.SetContributionStatus(ctx, row.ID, target.status, target.prNumber, target.prURL, row.Note); err != nil {
		return err
	}
	url := target.prURL
	if url == "" {
		url = row.URL
	}
	s.publish(row.BookID, row.Kind, target.status, url)
	return nil
}

// pollIssueMode advances an issue-mode row: it looks for the intake bot's PR
// (branch intake/issue-<n>); once found the row tracks that PR (pr_open ->
// merged), and an issue closed with no intake PR becomes closed.
func (s *Service) pollIssueMode(ctx context.Context, cli *Client, row store.Contribution) (rowTarget, error) {
	if row.PRNumber == 0 {
		pr, found, err := cli.FindIntakePR(ctx, row.Repo, row.Number)
		if err != nil {
			return rowTarget{}, err
		}
		if found {
			return prTarget(pr.Number, pr.URL, pr), nil
		}
		issue, err := cli.GetIssue(ctx, row.Repo, row.Number)
		if err != nil {
			return rowTarget{}, err
		}
		if issue.State == "closed" {
			return rowTarget{status: store.ContribStatusClosed}, nil
		}
		return rowTarget{status: row.Status}, nil // still open, no PR yet
	}
	// The intake PR is already known: track its merge/close.
	pr, err := cli.GetPull(ctx, row.Repo, row.PRNumber)
	if err != nil {
		return rowTarget{}, err
	}
	return prTarget(row.PRNumber, prURLOr(pr.URL, row.PRURL), pr), nil
}

// pollPRMode advances a pr-mode row (a direct sidecar PR): the row's own number is
// the PR, so it moves submitted -> pr_open (the PR exists) -> merged/closed. A
// pr-mode row carries no separate intake-PR pointer (its own number/url are the PR).
func (s *Service) pollPRMode(ctx context.Context, cli *Client, row store.Contribution) (rowTarget, error) {
	pr, err := cli.GetPull(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	return prTarget(0, "", pr), nil
}

// prTarget maps a PR's merge/close state to a rowTarget, carrying the given intake
// PR pointer (number/url).
func prTarget(prNumber int, prURL string, pr PR) rowTarget {
	switch {
	case pr.Merged:
		return rowTarget{status: store.ContribStatusMerged, prNumber: prNumber, prURL: prURL}
	case pr.State == "closed":
		return rowTarget{status: store.ContribStatusClosed, prNumber: prNumber, prURL: prURL}
	default:
		return rowTarget{status: store.ContribStatusPROpen, prNumber: prNumber, prURL: prURL}
	}
}

// prURLOr returns fresh when non-empty, else the fallback (so a re-poll keeps a
// known intake-PR url rather than blanking it).
func prURLOr(fresh, fallback string) string {
	if fresh != "" {
		return fresh
	}
	return fallback
}

// resolveCorePending finds every book parked core_pending whose core add-work PR
// has merged, reads the real work slug from that PR's created work.json, records it
// on the book, and re-admits the book. Failures are logged and skipped. client is the
// tick's memoized lazy resolver: it is invoked only once a book with a merged core PR
// is found, so an idle tick with no such work never resolves a credential.
func (s *Service) resolveCorePending(ctx context.Context, client func() *Client) {
	// Targeted work list: only the books whose merged add-work PR still needs its slug
	// resolved (no full-table scan, no per-book contribution query).
	books, err := s.deps.DB.ListBooksWithUnresolvedMergedCore(ctx)
	if err != nil {
		s.logf("contrib poller: list unresolved core books: %v", err)
		return
	}
	if len(books) == 0 {
		return
	}
	// One grouped contribution query supplies each candidate's core-row PR pointer.
	byBook, err := s.deps.DB.ContributionsByBook(ctx)
	if err != nil {
		s.logf("contrib poller: list contributions: %v", err)
		return
	}
	for _, b := range books {
		core, ok := findCore(byBook[b.ID])
		if !ok || core.Status != store.ContribStatusMerged || core.PRNumber == 0 {
			continue
		}
		files, err := client().PullFiles(ctx, core.Repo, core.PRNumber)
		if err != nil {
			s.logf("contrib poller: pull files for book %d PR %d: %v", b.ID, core.PRNumber, err)
			continue
		}
		slug := workSlugFromFiles(files)
		if slug == "" {
			s.logf("contrib poller: book %d core PR %d has no work.json path", b.ID, core.PRNumber)
			continue
		}
		// Persist the resolved slug regardless of the book's park state (idempotent): a
		// book that already left core_pending - e.g. a manual retry - still needs its
		// work id recorded so the contributing stage can attach the sidecars.
		if err := s.deps.DB.SetBookWorkID(ctx, b.ID, slug); err != nil {
			s.logf("contrib poller: set work id for book %d: %v", b.ID, err)
			continue
		}
		s.publish(b.ID, store.ContribKindCore, store.ContribStatusMerged, prURLOr(core.PRURL, core.URL))
		// Re-admit ONLY a book still parked core_pending; a book that already moved on
		// must not be re-admitted (that would rewind a running/paused book).
		if s.deps.Readmit != nil && state.IsParkedWith(b.Status, b.ParkCode, state.ParkCorePending) {
			if err := s.deps.Readmit(ctx, b.ID); err != nil {
				s.logf("contrib poller: re-admit book %d: %v", b.ID, err)
			}
		}
	}
}

// findCore returns the kind=core row from a book's contributions.
func findCore(rows []store.Contribution) (store.Contribution, bool) {
	for _, r := range rows {
		if r.Kind == store.ContribKindCore {
			return r, true
		}
	}
	return store.Contribution{}, false
}

// workSlugFromFiles extracts the created work slug from a PR's file list: the path
// data/works/<shard>/<slug>/work.json. It returns "" when no such path is present
// or the slug is not a valid meta slug.
func workSlugFromFiles(files []string) string {
	for _, f := range files {
		parts := strings.Split(strings.TrimPrefix(f, "/"), "/")
		if len(parts) != 5 {
			continue
		}
		if parts[0] != "data" || parts[1] != "works" || parts[4] != "work.json" {
			continue
		}
		slug := parts[3]
		if model.ValidSlug(slug) && model.Shard(slug) == parts[2] {
			return slug
		}
	}
	return ""
}
