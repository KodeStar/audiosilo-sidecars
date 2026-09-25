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

// verdictRecheckInterval spaces out re-checks of a row already carrying a
// needs-human/invalid verdict: it can wait on a human for days, and each check costs
// two GitHub calls against a 60/hour unauthenticated budget.
const verdictRecheckInterval = time.Hour

// rowTarget is the lifecycle state a poll tick wants to write onto a contribution
// row: the new status, note, and discovered intake-PR pointer (issue mode), plus the
// intake verdict it carries and whether that verdict is new to the row.
type rowTarget struct {
	status     string
	prNumber   int
	prURL      string
	note       string
	verdict    *intakeVerdict
	newVerdict bool
}

// RunPoller polls the upstream repos for open-contribution and core-pending state
// changes until ctx is cancelled, on a jittered interval so daemons do not hit
// GitHub in lockstep. Poll failures are logged and swallowed.
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

// jitter returns interval extended by a random 0..interval/4.
func jitter(interval time.Duration) time.Duration {
	span := int64(interval) / 4
	if span <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(span+1)) //nolint:gosec // jitter, not security
}

// Poll performs one tick: it advances every open contribution row against GitHub,
// then runs the release gate and learns the slug of every merged add-work PR. It
// works tokenless; one failed row is logged and skipped.
func (s *Service) Poll(ctx context.Context) {
	if s.deps.DB == nil {
		return
	}
	rows, err := s.deps.DB.ListOpenContributions(ctx)
	if err != nil {
		s.logf("contrib poller: list open contributions: %v", err)
		return
	}
	// The client is resolved lazily, at most once per tick: s.client can shell out
	// to `gh auth token`, which an idle tick must not pay for.
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
	for _, row := range rows {
		if s.verdictRecheckNotDue(row) {
			continue
		}
		if err := s.advanceRow(ctx, client(), row); err != nil {
			s.logf("contrib poller: advance book %d %s: %v", row.BookID, row.Kind, err)
		}
	}
	// Release gate first (slug known), then slug learning (slug not known), so a
	// book resolved this tick is checked once.
	s.releaseCorePending(ctx)
	s.resolveCorePending(ctx, client)
}

// verdictRecheckNotDue reports whether a row waiting on a human (a needs-human or
// invalid verdict) was checked less than verdictRecheckInterval ago.
func (s *Service) verdictRecheckNotDue(row store.Contribution) bool {
	if !strings.Contains(row.Note, store.ContribNoteIntakeNeedsHuman) && !strings.Contains(row.Note, store.ContribNoteIntakeInvalid) {
		return false
	}
	checked, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	return err == nil && s.now().Before(checked.Add(verdictRecheckInterval))
}

// advanceRow computes a row's new state from GitHub and persists it only when
// something changed, publishing a contrib.update. An unchanged row waiting on a
// verdict is touched instead, which is what spaces its re-checks.
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
		if target.verdict != nil && target.status == store.ContribStatusSubmitted {
			return s.deps.DB.TouchContribution(ctx, row.ID)
		}
		return nil // steady state: no persist, no publish
	}
	if err := s.deps.DB.SetContributionStatus(ctx, row.ID, target.status, target.prNumber, target.prURL, target.note); err != nil {
		return err
	}
	s.publish(row.BookID, row.Kind, target.status, prURLOr(target.prURL, row.URL))
	if row.Kind == store.ContribKindCore && target.newVerdict {
		s.coreVerdictFollowUp(ctx, row, *target.verdict)
	}
	return nil
}

// coreVerdictFollowUp moves a core_pending book on when its add-work issue got a
// verdict instead of a PR: a duplicate re-admits it (the stage re-resolves the work,
// or asks a human to set it); needs-human/invalid re-word its park message.
func (s *Service) coreVerdictFollowUp(ctx context.Context, row store.Contribution, v intakeVerdict) {
	b, err := s.deps.DB.GetBook(ctx, row.BookID)
	if err != nil || !state.IsParkedWith(b.Status, b.ParkCode, state.ParkCorePending) {
		return
	}
	if v.note == store.ContribNoteIntakeDuplicate {
		if s.deps.Readmit != nil {
			if err := s.deps.Readmit(ctx, b.ID); err != nil {
				s.logf("contrib poller: re-admit book %d after a duplicate verdict: %v", b.ID, err)
			}
		}
		return
	}
	msg := fmt.Sprintf(CoreVerdictMsgFormat, strings.TrimPrefix(v.note, store.ContribNoteIntakeVerdictPrefix), row.URL)
	if err := s.deps.DB.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), msg, string(state.ParkCorePending)); err != nil {
		s.logf("contrib poller: note core verdict for book %d: %v", b.ID, err)
	}
}

// CoreVerdictMsgFormat is the park message of a core_pending book whose add-work
// issue the intake bot answered needs-human or invalid: the verdict, the issue URL.
const CoreVerdictMsgFormat = "the work proposal was answered %q by the intake bot - see %s; fix the issue (an edit re-runs the bot) or set the book's work by hand"

// pollIssueMode advances an issue-mode row: it follows the intake bot's PR (branch
// intake/issue-<n>) through pr_open -> merged/closed; without one it surfaces the
// bot's verdict, closes a closed issue, or notes an overdue one.
func (s *Service) pollIssueMode(ctx context.Context, cli *Client, row store.Contribution) (rowTarget, error) {
	base := withoutVerdictNote(withoutStaleIntakeNote(row.Note))
	if row.PRNumber != 0 {
		pr, err := cli.GetPull(ctx, row.Repo, row.PRNumber)
		if err != nil {
			return rowTarget{}, err
		}
		return prTarget(row.PRNumber, prURLOr(pr.URL, row.PRURL), pr, withoutStaleIntakeNote(row.Note)), nil
	}
	pr, found, err := cli.FindIntakePR(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	if found {
		return prTarget(pr.Number, pr.URL, pr, base), nil
	}
	issue, err := cli.GetIssue(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	if v, ok := verdictFromLabels(issue.Labels); ok {
		return s.verdictTarget(ctx, cli, row, issue, v, base)
	}
	if issue.State == "closed" {
		return rowTarget{status: store.ContribStatusClosed, note: base}, nil
	}
	note := withoutVerdictNote(row.Note)
	if issueWithoutPRStale(row.UpdatedAt, s.now()) {
		note = withStaleIntakeNote(note)
	}
	return rowTarget{status: row.Status, note: note}, nil // still open, no PR yet
}

// intakeVerdict is an outcome the intake bot labels an issue with when it opens no
// pull request (intake.yml, in both the core and the community repo).
type intakeVerdict struct {
	label string // the verdict label the bot applies
	note  string // the row-note segment (a store.ContribNoteIntake* marker)
}

// intakeVerdicts in precedence order: a duplicate is settled, so it wins over a
// needs-human label left from an earlier run.
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

// intakeBotMarker closes every verdict comment the intake bot posts.
const intakeBotMarker = "_Posted by the intake bot._"

// maxVerdictDetail bounds the bot's own messages carried into a row note.
const maxVerdictDetail = 400

// verdictTarget maps an intake verdict to the row's new state: a duplicate is
// already_covered (upstream already, done by someone else); needs-human/invalid stay
// submitted with an actionable note. The bot comment's message lines are read only
// when the verdict is new to the row.
func (s *Service) verdictTarget(ctx context.Context, cli *Client, row store.Contribution, issue Issue, v intakeVerdict, base string) (rowTarget, error) {
	t := rowTarget{status: store.ContribStatusSubmitted, verdict: &v}
	if v.note == store.ContribNoteIntakeDuplicate {
		t.status = store.ContribStatusAlreadyCovered
	} else if issue.State == "closed" {
		t.status = store.ContribStatusClosed
	}
	if i := strings.Index(row.Note, v.note); i >= 0 {
		t.note = JoinNotes(base, row.Note[i:])
		return t, nil
	}
	segment := v.note
	comments, err := cli.IssueComments(ctx, row.Repo, row.Number)
	if err != nil {
		return rowTarget{}, err
	}
	if detail := latestBotDetail(comments); detail != "" {
		segment += " - " + detail
	}
	t.note, t.newVerdict = JoinNotes(base, segment), true
	return t, nil
}

// latestBotDetail returns the "- " message lines of the intake bot's most recent
// verdict comment, joined and bounded, or "" when there is none.
func latestBotDetail(comments []IssueComment) string {
	for i := len(comments) - 1; i >= 0; i-- {
		body := comments[i].Body
		if !strings.Contains(body, intakeBotMarker) {
			continue
		}
		var lines []string
		for _, l := range strings.Split(body, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "- ") {
				lines = append(lines, strings.TrimSpace(l[2:]))
			}
		}
		return truncateRunes(strings.Join(lines, " / "), maxVerdictDetail)
	}
	return ""
}

// withoutVerdictNote drops a recorded intake verdict - always the note's last
// segment - so a fresh verdict, or the intake PR appearing after all, replaces it.
func withoutVerdictNote(note string) string {
	i := strings.Index(note, store.ContribNoteIntakeVerdictPrefix)
	if i < 0 {
		return note
	}
	return strings.TrimSuffix(strings.TrimSpace(note[:i]), ";")
}

// pollPRMode advances a row recorded in the retired pr mode (a direct sidecar PR):
// its own number is the PR, so it moves pr_open -> merged/closed.
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
	return JoinNotes(note, store.ContribNoteIntakePRStale)
}

func withoutStaleIntakeNote(note string) string {
	note = strings.ReplaceAll(note, "; "+store.ContribNoteIntakePRStale, "")
	note = strings.ReplaceAll(note, store.ContribNoteIntakePRStale+"; ", "")
	if note == store.ContribNoteIntakePRStale {
		return ""
	}
	return note
}

// prTarget maps a PR's merge/close state to a rowTarget carrying the given intake-PR
// pointer.
func prTarget(prNumber int, prURL string, pr PR, note string) rowTarget {
	t := rowTarget{status: store.ContribStatusPROpen, prNumber: prNumber, prURL: prURL, note: note}
	switch {
	case pr.Merged:
		t.status = store.ContribStatusMerged
	case pr.State == "closed":
		t.status = store.ContribStatusClosed
	}
	return t
}

// prURLOr returns fresh when non-empty, else the fallback.
func prURLOr(fresh, fallback string) string {
	if fresh != "" {
		return fresh
	}
	return fallback
}

// resolveCorePending learns the new work's slug for every book whose add-work PR
// merged but whose work_id is unknown (learnCreatedWork), records it, and re-admits
// the book once the work is live (admitWhenLive). client is the tick's lazy resolver.
func (s *Service) resolveCorePending(ctx context.Context, client func() *Client) {
	books, err := s.deps.DB.ListBooksWithUnresolvedMergedCore(ctx)
	if err != nil {
		s.logf("contrib poller: list unresolved core books: %v", err)
		return
	}
	if len(books) == 0 {
		return
	}
	byBook, err := s.deps.DB.ContributionsByBook(ctx)
	if err != nil {
		s.logf("contrib poller: list contributions: %v", err)
		return
	}
	for _, b := range books {
		core, ok := findCore(byBook[b.ID])
		// A row with no intake PR (hand-merged) has nothing to read, and a PR already
		// found not to name exactly one work waits for a human (Set work).
		if !ok || core.Status != store.ContribStatusMerged || core.PRNumber == 0 ||
			strings.Contains(core.Note, store.ContribNoteCoreSlugUnresolvedPrefix) {
			continue
		}
		slug, why, err := learnCreatedWork(ctx, client(), core.Repo, core.PRNumber)
		if err != nil {
			s.logf("contrib poller: book %d core PR %d: %v", b.ID, core.PRNumber, err)
			continue
		}
		if slug == "" {
			note := JoinNotes(core.Note, store.ContribNoteCoreSlugUnresolvedPrefix+" ("+why+") - set the book's work by hand")
			if err := s.deps.DB.SetContributionStatus(ctx, core.ID, core.Status, core.PRNumber, core.PRURL, note); err != nil {
				s.logf("contrib poller: note unresolved core slug for book %d: %v", b.ID, err)
			}
			s.publish(b.ID, store.ContribKindCore, core.Status, prURLOr(core.PRURL, core.URL))
			continue
		}
		// Recorded regardless of park state: a book that already left core_pending
		// still needs its work id for the contributing stage.
		if err := s.deps.DB.SetBookWorkID(ctx, b.ID, slug); err != nil {
			s.logf("contrib poller: set work id for book %d: %v", b.ID, err)
			continue
		}
		s.publish(b.ID, store.ContribKindCore, store.ContribStatusMerged, prURLOr(core.PRURL, core.URL))
		b.WorkID = slug
		s.admitWhenLive(ctx, b)
	}
}

// releaseCorePending re-checks every core_pending book whose merged work's slug is
// known, re-admitting each once a data release holds the work.
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

// admitWhenLive is the RELEASE GATE: a core_pending book is re-admitted only once
// the published catalogue holds its work (the community intake verifies a sidecar's
// key against the newest release), adopting a surviving slug. Until then its park
// message says it waits for a release; a transport failure waits for the next tick.
func (s *Service) admitWhenLive(ctx context.Context, b store.Book) {
	pending := state.IsParkedWith(b.Status, b.ParkCode, state.ParkCorePending)
	if s.deps.ResolveWork != nil {
		live, err := s.deps.ResolveWork(ctx, b.WorkID)
		switch {
		case err == nil:
			if _, err := AdoptLiveWork(ctx, s.deps.DB, b.ID, b.WorkID, live); err != nil {
				s.logf("contrib poller: adopt surviving work id for book %d: %v", b.ID, err)
				return
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

// isWorksPack reports whether a repository path is a core works-family pack
// (data/works/<dir-bound>/<bound>.json, audiosilo-meta PACK-SPEC.md).
func isWorksPack(path string) bool {
	return strings.HasPrefix(path, "data/works/") && strings.HasSuffix(path, ".json")
}

// learnCreatedWork reads which work a merged add-work PR created, from the PR's OWN
// change: compare base.sha...head.sha, then the entry keys of every works pack it
// touched at the merge base vs head.sha (refs/pull/N/head keeps head reachable), one
// key set per side, so a pack split's moved keys cancel out. It is independent of the
// merge style. One new key is the answer; zero or several is a reason, never a guess.
// err is transient (the tick retries).
func learnCreatedWork(ctx context.Context, cli *Client, repo string, prNumber int) (slug, why string, err error) {
	pr, err := cli.GetPull(ctx, repo, prNumber)
	if err != nil {
		return "", "", err
	}
	if !pr.Merged || pr.BaseSHA == "" || pr.HeadSHA == "" {
		return "", "", errors.New("merged PR carries no base/head sha")
	}
	cmp, err := cli.Compare(ctx, repo, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return "", "", err
	}
	if cmp.MergeBase == "" {
		return "", "", errors.New("compare returned no merge base")
	}
	paths := map[string]bool{}
	for _, f := range cmp.Files {
		for _, p := range []string{f.Filename, f.PreviousFilename} {
			if isWorksPack(p) {
				paths[p] = true
			}
		}
	}
	if len(paths) == 0 {
		return "", "the PR changed no data/works pack file", nil
	}
	before, after := map[string]bool{}, map[string]bool{}
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		for ref, keys := range map[string]map[string]bool{cmp.MergeBase: before, pr.HeadSHA: after} {
			raw, found, err := cli.FileAt(ctx, repo, p, ref)
			if err != nil {
				return "", "", err
			}
			if !found {
				continue // absent on this side (added / removed)
			}
			f, err := pack.Parse(raw)
			if err != nil {
				return "", fmt.Sprintf("%s at %.7s is not a readable pack: %v", p, ref, err), nil
			}
			for _, k := range f.Slugs() {
				keys[k] = true
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
		return "", fmt.Sprintf("%d new work entries: %s", len(added), strings.Join(added[:min(len(added), 5)], ", ")), nil
	case !model.ValidSlug(added[0]):
		return "", fmt.Sprintf("new entry key %q is not a valid slug", added[0]), nil
	}
	return added[0], "", nil
}
