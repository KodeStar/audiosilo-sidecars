package contrib

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/model"
	"github.com/kodestar/audiosilo-meta/pkg/pack"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// defaultPollInterval is the fallback poll cadence when the configured interval is
// non-positive.
const defaultPollInterval = 10 * time.Minute

// intakePRGracePeriod gives the metadata intake workflow ample time to start,
// compose, and open its PR before an issue-without-PR is surfaced as stalled.
const intakePRGracePeriod = 30 * time.Minute

// rowTarget is the lifecycle state a poll tick wants to write onto a contribution
// row: the new status, note, and discovered intake-PR pointer (issue mode).
type rowTarget struct {
	status   string
	prNumber int
	prURL    string
	note     string
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

	// The release gate first (books whose slug is known), then the slug learning
	// (books whose slug is not): a book resolved this tick is checked once, by the
	// second pass, rather than twice.
	s.releaseCorePending(ctx)
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
	if target.status == row.Status && target.prNumber == row.PRNumber && target.prURL == row.PRURL && target.note == row.Note {
		return nil // steady state: no persist, no publish
	}
	if err := s.deps.DB.SetContributionStatus(ctx, row.ID, target.status, target.prNumber, target.prURL, target.note); err != nil {
		return err
	}
	url := target.prURL
	if url == "" {
		url = row.URL
	}
	s.publish(row.BookID, row.Kind, target.status, url)
	if row.Kind == store.ContribKindCore {
		s.coreVerdictFollowUp(ctx, row, target)
	}
	return nil
}

// coreVerdictFollowUp moves a core_pending book on when the intake bot answered its
// add-work issue with a verdict instead of a pull request - otherwise the book would
// wait on a PR that is never coming. A duplicate (the work is in the catalogue
// already) re-admits it, so the contributing stage re-resolves the work (an
// identifier match, or core_needed asking a human to set it); needs-human and
// invalid re-word its park message to point at the issue. Only a NEW verdict acts.
func (s *Service) coreVerdictFollowUp(ctx context.Context, row store.Contribution, target rowTarget) {
	var verdict string
	for _, v := range intakeVerdicts {
		if strings.Contains(target.note, v.note) && !strings.Contains(row.Note, v.note) {
			verdict = v.note
		}
	}
	if verdict == "" {
		return
	}
	b, err := s.deps.DB.GetBook(ctx, row.BookID)
	if err != nil || !state.IsParkedWith(b.Status, b.ParkCode, state.ParkCorePending) {
		return
	}
	if verdict == store.ContribNoteIntakeDuplicate {
		if s.deps.Readmit != nil {
			if err := s.deps.Readmit(ctx, b.ID); err != nil {
				s.logf("contrib poller: re-admit book %d after a duplicate verdict: %v", b.ID, err)
			}
		}
		return
	}
	msg := fmt.Sprintf(CoreVerdictMsgFormat, strings.TrimPrefix(verdict, store.ContribNoteIntakeVerdictPrefix), row.URL)
	if err := s.deps.DB.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), msg, string(state.ParkCorePending)); err != nil {
		s.logf("contrib poller: note core verdict for book %d: %v", b.ID, err)
	}
}

// CoreVerdictMsgFormat is the park message of a core_pending book whose add-work
// issue the intake bot answered needs-human or invalid: %s is the verdict, %s the
// issue URL.
const CoreVerdictMsgFormat = "the work proposal was answered %q by the intake bot - see %s; fix the issue (an edit re-runs the bot) or set the book's work by hand"

// pollIssueMode advances an issue-mode row: it looks for the intake bot's PR
// (branch intake/issue-<n>); once found the row tracks that PR (pr_open ->
// merged). Without one, the issue's intake VERDICT is surfaced when the bot gave
// one (intakeVerdict), an issue closed with no intake PR becomes closed, and an
// open one past the workflow window is noted as overdue.
func (s *Service) pollIssueMode(ctx context.Context, cli *Client, row store.Contribution) (rowTarget, error) {
	if row.PRNumber == 0 {
		pr, found, err := cli.FindIntakePR(ctx, row.Repo, row.Number)
		if err != nil {
			return rowTarget{}, err
		}
		if found {
			return prTarget(pr.Number, pr.URL, pr, withoutVerdictNote(withoutStaleIntakeNote(row.Note))), nil
		}
		issue, err := cli.GetIssue(ctx, row.Repo, row.Number)
		if err != nil {
			return rowTarget{}, err
		}
		if v, ok := verdictFromLabels(issue.Labels); ok {
			return s.verdictTarget(ctx, cli, row, issue, v)
		}
		base := withoutVerdictNote(withoutStaleIntakeNote(row.Note))
		if issue.State == "closed" {
			return rowTarget{status: store.ContribStatusClosed, note: base}, nil
		}
		note := withoutVerdictNote(row.Note)
		if issueWithoutPRStale(row.UpdatedAt, s.now()) {
			note = withStaleIntakeNote(note)
		}
		return rowTarget{status: row.Status, note: note}, nil // still open, no PR yet
	}
	// The intake PR is already known: track its merge/close.
	pr, err := cli.GetPull(ctx, row.Repo, row.PRNumber)
	if err != nil {
		return rowTarget{}, err
	}
	return prTarget(row.PRNumber, prURLOr(pr.URL, row.PRURL), pr, withoutStaleIntakeNote(row.Note)), nil
}

// intakeVerdict is one of the outcomes the metadata repositories' intake bot labels
// an issue with when it does NOT open a pull request (intake.yml's "Comment the
// outcome and label the issue" step, in both the core and the community repo).
type intakeVerdict struct {
	label string // the verdict label the bot applies
	note  string // the row-note segment (a store.ContribNoteIntake* marker)
}

// intakeVerdicts in precedence order: a duplicate is settled (the thing is upstream
// already), so it wins over a stale needs-human label left from an earlier run.
var intakeVerdicts = []intakeVerdict{
	{label: "data:duplicate", note: store.ContribNoteIntakeDuplicate},
	{label: "data:needs-human", note: store.ContribNoteIntakeNeedsHuman},
	{label: "data:invalid", note: store.ContribNoteIntakeInvalid},
}

// verdictFromLabels returns the verdict an issue's labels carry, if any.
func verdictFromLabels(labels []string) (intakeVerdict, bool) {
	for _, v := range intakeVerdicts {
		if slices.Contains(labels, v.label) {
			return v, true
		}
	}
	return intakeVerdict{}, false
}

// intakeBotMarker closes every verdict comment the intake bot posts; it is how the
// bot's comment is told from a human's on the same issue.
const intakeBotMarker = "_Posted by the intake bot._"

// maxVerdictDetail bounds the bot's own messages carried into a row note.
const maxVerdictDetail = 400

// verdictTarget maps an intake verdict to the row's new state. A duplicate is
// recorded already_covered - the work or sidecar is in the database already, done by
// someone else - and needs-human / invalid keep the row submitted with an actionable
// note (the issue stays open, and an edit re-runs the bot, so the row keeps being
// polled). The note's detail is the bot's own message lines from its latest verdict
// comment, fetched only when the verdict is new to this row.
func (s *Service) verdictTarget(ctx context.Context, cli *Client, row store.Contribution, issue Issue, v intakeVerdict) (rowTarget, error) {
	base := withoutVerdictNote(withoutStaleIntakeNote(row.Note))
	status := store.ContribStatusSubmitted
	if v.note == store.ContribNoteIntakeDuplicate {
		status = store.ContribStatusAlreadyCovered
	} else if issue.State == "closed" {
		status = store.ContribStatusClosed
	}
	// The same verdict already recorded: keep its detail rather than re-reading the
	// comments every tick.
	if i := strings.Index(row.Note, v.note); i >= 0 {
		return rowTarget{status: status, note: joinNote(base, row.Note[i:])}, nil
	}
	segment := v.note
	comments, err := cli.IssueComments(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	if detail := latestBotDetail(comments); detail != "" {
		segment += " - " + detail
	}
	return rowTarget{status: status, note: joinNote(base, segment)}, nil
}

// latestBotDetail returns the message lines ("- ..." bullets) of the intake bot's
// most recent verdict comment, joined and bounded, or "" when there is none.
func latestBotDetail(comments []IssueComment) string {
	for i := len(comments) - 1; i >= 0; i-- {
		body := comments[i].Body
		if !strings.Contains(body, intakeBotMarker) {
			continue
		}
		var lines []string
		for _, l := range strings.Split(body, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "- ") {
				lines = append(lines, strings.TrimSpace(l[2:]))
			}
		}
		detail := strings.Join(lines, " / ")
		if len(detail) > maxVerdictDetail {
			cut := maxVerdictDetail
			for cut > 0 && !utf8.RuneStart(detail[cut]) {
				cut--
			}
			detail = detail[:cut] + "..."
		}
		return detail
	}
	return ""
}

// withoutVerdictNote drops a recorded intake verdict - always the note's LAST
// segment - so a fresh verdict, or the intake PR appearing after all, replaces it.
func withoutVerdictNote(note string) string {
	i := strings.Index(note, store.ContribNoteIntakeVerdictPrefix)
	if i < 0 {
		return note
	}
	return strings.TrimSuffix(strings.TrimSpace(note[:i]), ";")
}

// pollPRMode advances a pr-mode row (a direct sidecar PR): the row's own number is
// the PR, so it moves submitted -> pr_open (the PR exists) -> merged/closed. A
// pr-mode row carries no separate intake-PR pointer (its own number/url are the PR).
func (s *Service) pollPRMode(ctx context.Context, cli *Client, row store.Contribution) (rowTarget, error) {
	pr, err := cli.GetPull(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	return prTarget(0, "", pr, row.Note), nil
}

func issueWithoutPRStale(lastUpdatedAt string, now time.Time) bool {
	lastUpdated, err := time.Parse(time.RFC3339Nano, lastUpdatedAt)
	return err == nil && !now.Before(lastUpdated.Add(intakePRGracePeriod))
}

func withStaleIntakeNote(note string) string {
	if strings.Contains(note, store.ContribNoteIntakePRStale) {
		return note
	}
	if strings.TrimSpace(note) == "" {
		return store.ContribNoteIntakePRStale
	}
	return note + "; " + store.ContribNoteIntakePRStale
}

func withoutStaleIntakeNote(note string) string {
	note = strings.ReplaceAll(note, "; "+store.ContribNoteIntakePRStale, "")
	note = strings.ReplaceAll(note, store.ContribNoteIntakePRStale+"; ", "")
	if note == store.ContribNoteIntakePRStale {
		return ""
	}
	return note
}

// prTarget maps a PR's merge/close state to a rowTarget, carrying the given intake
// PR pointer (number/url).
func prTarget(prNumber int, prURL string, pr PR, note string) rowTarget {
	switch {
	case pr.Merged:
		return rowTarget{status: store.ContribStatusMerged, prNumber: prNumber, prURL: prURL, note: note}
	case pr.State == "closed":
		return rowTarget{status: store.ContribStatusClosed, prNumber: prNumber, prURL: prURL, note: note}
	default:
		return rowTarget{status: store.ContribStatusPROpen, prNumber: prNumber, prURL: prURL, note: note}
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

// resolveCorePending finds every book whose core add-work PR has merged but whose
// work slug is not known yet, learns the new work's slug from the pack files that PR
// changed (learnCreatedWork), records it on the book, and re-admits the book once the
// published catalogue holds the work (admitWhenLive). Failures are logged and
// skipped. client is the tick's memoized lazy resolver: it is invoked only once a
// book with a merged core PR is found, so an idle tick never resolves a credential.
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
		if !ok || core.Status != store.ContribStatusMerged {
			continue
		}
		// An issue-mode core row tracks its intake PR in PRNumber; a row recorded
		// without one (a hand-merged PR) has nothing to read.
		prNumber := core.PRNumber
		if prNumber == 0 {
			continue
		}
		// A PR already found not to name exactly one new work waits for a human
		// (Set work); re-reading the same merged PR every tick cannot change that.
		if strings.Contains(core.Note, store.ContribNoteCoreSlugUnresolvedPrefix) {
			continue
		}
		slug, why, err := learnCreatedWork(ctx, client(), core.Repo, prNumber)
		if err != nil {
			s.logf("contrib poller: book %d core PR %d: %v", b.ID, prNumber, err)
			continue
		}
		if slug == "" {
			note := joinNote(core.Note, store.ContribNoteCoreSlugUnresolvedPrefix+" ("+why+") - set the book's work by hand")
			if err := s.deps.DB.SetContributionStatus(ctx, core.ID, core.Status, core.PRNumber, core.PRURL, note); err != nil {
				s.logf("contrib poller: note unresolved core slug for book %d: %v", b.ID, err)
			}
			s.publish(b.ID, store.ContribKindCore, core.Status, prURLOr(core.PRURL, core.URL))
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
		b.WorkID = slug
		s.admitWhenLive(ctx, b)
	}
}

// releaseCorePending is the release gate's retry loop: every book parked core_pending
// whose add-work PR merged and whose slug is already known is re-checked against the
// published catalogue, and re-admitted once a data release holds the work.
func (s *Service) releaseCorePending(ctx context.Context) {
	books, err := s.deps.DB.ListBooksAwaitingRelease(ctx)
	if err != nil {
		s.logf("contrib poller: list books awaiting a release: %v", err)
		return
	}
	for _, b := range books {
		s.admitWhenLive(ctx, b)
	}
}

// admitWhenLive re-admits a core_pending book whose work slug is recorded, but only
// once the published catalogue holds that work: a merged add-work PR is not yet in
// any data release, and the community intake verifies a sidecar's key against the
// newest release - contributing before then is refused there. A slug the catalogue
// answers under a different (surviving) slug is adopted. Until the work is live the
// book's park message says it is waiting for the next data release; a transport
// failure leaves everything for the next tick. A book that is not parked
// core_pending is never re-admitted (that would rewind a running book).
func (s *Service) admitWhenLive(ctx context.Context, b store.Book) {
	pending := state.IsParkedWith(b.Status, b.ParkCode, state.ParkCorePending)
	if s.deps.ResolveWork != nil {
		live, err := s.deps.ResolveWork(ctx, b.WorkID)
		switch {
		case err == nil:
			if model.ValidSlug(live) && live != b.WorkID {
				if err := s.deps.DB.SetBookWorkID(ctx, b.ID, live); err != nil {
					s.logf("contrib poller: adopt surviving work id for book %d: %v", b.ID, err)
					return
				}
			}
		case errors.Is(err, ErrWorkNotFound):
			if pending && b.Error != ReleaseWaitMsg {
				if err := s.deps.DB.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention),
					ReleaseWaitMsg, string(state.ParkCorePending)); err != nil {
					s.logf("contrib poller: note release wait for book %d: %v", b.ID, err)
				}
			}
			return
		default:
			s.logf("contrib poller: check work %q for book %d: %v", b.WorkID, b.ID, err)
			return
		}
	}
	if pending && s.deps.Readmit != nil {
		if err := s.deps.Readmit(ctx, b.ID); err != nil {
			s.logf("contrib poller: re-admit book %d: %v", b.ID, err)
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

// worksPackPrefix is the core repository's works family: range-packed files
// data/works/<dir-bound>/<bound>.json, each {"entries": {"<work-slug>": {...}}}
// (audiosilo-meta PACK-SPEC.md). A path only names a pack, never a work.
const worksPackPrefix = "data/works/"

// isWorksPack reports whether a repository path is a works-family pack file.
func isWorksPack(path string) bool {
	return strings.HasPrefix(path, worksPackPrefix) && strings.HasSuffix(path, ".json")
}

// learnCreatedWork reads which work a merged add-work PR created.
//
// The works family is range-packed, so a pack path names a slug RANGE, not a work,
// and a write re-renders its pack and may SPLIT it (entries move into new files and
// the old file is renamed or removed). So the answer is read from the ENTRY KEYS,
// across every works pack the change touched taken as ONE set on each side: the keys
// after the PR landed minus the keys just before it. A key that merely moved
// between packs is on both sides and cancels out. Exactly one new key is the
// created work; zero or several yield "" with the reason, and nothing is guessed.
//
// The two sides are the PR's net change ON THE BASE BRANCH (prChangeRange), which
// is exact in every merge style, and the files are the compare API's list between
// them. err is a transient failure (the tick retries); a "" slug with a reason is a
// settled answer.
func learnCreatedWork(ctx context.Context, cli *Client, repo string, prNumber int) (slug, why string, err error) {
	pr, err := cli.GetPull(ctx, repo, prNumber)
	if err != nil {
		return "", "", err
	}
	if !pr.Merged || pr.MergeCommitSHA == "" {
		return "", "", errors.New("merged PR carries no merge commit yet")
	}
	base, why, err := prChangeRange(ctx, cli, repo, pr)
	if err != nil || why != "" {
		return "", why, err
	}
	files, err := cli.CompareFiles(ctx, repo, base, pr.MergeCommitSHA)
	if err != nil {
		return "", "", err
	}
	paths := map[string]bool{}
	for _, f := range files {
		if isWorksPack(f.Filename) {
			paths[f.Filename] = true
		}
		if isWorksPack(f.PreviousFilename) {
			paths[f.PreviousFilename] = true
		}
	}
	if len(paths) == 0 {
		return "", "the PR changed no data/works pack file", nil
	}
	before, after := map[string]bool{}, map[string]bool{}
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		for _, side := range []struct {
			ref  string
			keys map[string]bool
		}{{base, before}, {pr.MergeCommitSHA, after}} {
			raw, found, err := cli.FileAt(ctx, repo, p, side.ref)
			if err != nil {
				return "", "", err
			}
			if !found {
				continue // the file does not exist on this side (added / removed)
			}
			f, err := pack.Parse(raw)
			if err != nil {
				return "", fmt.Sprintf("%s at %.7s is not a readable pack: %v", p, side.ref, err), nil
			}
			for _, k := range f.Slugs() {
				side.keys[k] = true
			}
		}
	}
	var added []string
	for k := range after {
		if !before[k] {
			added = append(added, k)
		}
	}
	slices.Sort(added)
	switch {
	case len(added) == 0:
		return "", "no new work entry", nil
	case len(added) > 1:
		shown := added
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return "", fmt.Sprintf("%d new work entries: %s", len(added), strings.Join(shown, ", ")), nil
	case !model.ValidSlug(added[0]):
		return "", fmt.Sprintf("new entry key %q is not a valid slug", added[0]), nil
	}
	return added[0], "", nil
}

// maxRebaseWalk bounds how many commits prChangeRange walks back for a rebase merge
// (GitHub lists at most 250 commits on a pull request).
const maxRebaseWalk = 250

// prChangeRange returns the base-branch commit just before a merged PR's change,
// so that base..pr.MergeCommitSHA is EXACTLY the PR's net change, in each merge
// style GitHub offers:
//
//   - a merge commit (two parents): its first parent - the base branch before;
//   - a squash merge: one new commit carrying the whole change, whose parent is
//     the base branch before;
//   - a rebase merge: the PR's N commits re-created on the base branch, the merge
//     sha being the LAST of them, so the base is N commits back. Diffing only the
//     last commit would miss an entry an earlier commit added.
//
// A squash and a rebase both leave single-parent commits, so the two are told
// apart by what a rebase preserves: the re-created commits carry the PR commits'
// messages and author dates, in order. Walking back from the merge sha, a chain
// matching every PR commit is a rebase; anything else is a squash (and for a
// one-commit PR the two readings agree). why is set when the history cannot be
// read as either.
func prChangeRange(ctx context.Context, cli *Client, repo string, pr PR) (base, why string, err error) {
	merge, err := cli.GetCommit(ctx, repo, pr.MergeCommitSHA)
	if err != nil {
		return "", "", err
	}
	if len(merge.Parents) == 0 {
		return "", "the merge commit has no parent", nil
	}
	if len(merge.Parents) > 1 || pr.Commits <= 1 {
		return merge.Parents[0], "", nil
	}
	commits, err := cli.PullCommits(ctx, repo, pr.Number)
	if err != nil {
		return "", "", err
	}
	if len(commits) == 0 || len(commits) > maxRebaseWalk {
		return merge.Parents[0], "", nil
	}
	cur := merge
	for i := len(commits) - 1; i >= 0; i-- {
		if cur.Message != commits[i].Message || cur.AuthorDate != commits[i].AuthorDate || len(cur.Parents) != 1 {
			return merge.Parents[0], "", nil // not the PR's commits re-created: a squash
		}
		if i == 0 {
			return cur.Parents[0], "", nil // rebase: the parent of the PR's first commit
		}
		if cur, err = cli.GetCommit(ctx, repo, cur.Parents[0]); err != nil {
			return "", "", err
		}
	}
	return merge.Parents[0], "", nil
}

// joinNote appends part to a "; "-joined row note.
func joinNote(note, part string) string {
	if strings.TrimSpace(note) == "" {
		return part
	}
	return note + "; " + part
}
