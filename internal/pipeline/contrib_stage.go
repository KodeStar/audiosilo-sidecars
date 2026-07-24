package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/canonical"
	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/contrib"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/metaops"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// MetaCoverage is the slice of the metaops client the contributing stage needs:
// resolve a book identity to a work (asin/isbn lookup) and read a work's sidecar
// coverage. *metaops.Client satisfies it; it is an interface so the stage tests can
// point it at an httptest meta server (or leave it nil = metadata disabled).
type MetaCoverage interface {
	CoverageFor(context.Context, metaops.BookIdentity) (metaops.Coverage, error)
	CoverageForWork(context.Context, string) (metaops.Coverage, error)
}

// TokenResolver resolves a GitHub credential for the issue/pr contribution modes.
// *contrib.TokenSource satisfies it; it is an interface so tests inject a
// deterministic resolver (real `gh auth token` on the host would make a
// no-credential test flaky).
type TokenResolver interface {
	Resolve(ctx context.Context) (token, from string, err error)
}

// Contribution modes (mirrors config.ContributionMode*; kept as local literals so
// the pipeline package does not import internal/config, matching how AgentModels is
// a plain-map view of the agent config).
const (
	contribModeIssue = "issue"
	contribModePR    = "pr"
	contribModeLocal = "local"
)

// defaultContribBaseURL is the GitHub REST base the contrib client uses when the
// executor was built without an explicit override (tests point it at httptest).
const defaultContribBaseURL = "https://api.github.com"

// Contributing-stage park messages (exported so the UI and tests can assert them
// exactly, mirroring AgentUnavailableMsg / MediaToolsUnavailableMsg).
const (
	// ContribUnavailableMsg parks a book in issue/pr mode when no GitHub credential
	// is available (no PAT in secrets, no `gh auth token`).
	ContribUnavailableMsg = "GitHub credential unavailable - add a personal access token in Settings or run `gh auth login`, then Retry"
	// CoreNeededMsg parks a book whose work does not exist upstream: a core add-work
	// proposal has been prefilled and awaits completion/confirmation in the UI.
	CoreNeededMsg = "this book's work does not exist on AudioSilo Meta yet - complete the work proposal, then it contributes automatically"
	// CorePendingMsg parks a book whose core proposal has been submitted: it waits for
	// the intake PR to merge, after which the poller resolves the slug and re-admits it.
	CorePendingMsg = "the work proposal has been submitted - waiting for the metadata PR to merge; the book resumes automatically"
)

// Contributing-stage work-dir layout + provenance constants.
const (
	contribDir       = "contrib"
	coreProposalName = "core_proposal.json"
	// coreProvenanceLine is the honest, short Sources line the prefilled core
	// proposal carries (the folder scan is the origin of the factual fields).
	coreProvenanceLine = "audiosilo-sidecars folder scan (embedded tags)"
)

// contribArtifact is one sidecar dimension to contribute (characters or recaps): the
// store kind, the on-disk sidecar path, and whether the resolved work already carries
// that dimension upstream (covered => recorded already_covered, never submitted).
type contribArtifact struct {
	kind    string // store.ContribKind{Characters,Recaps}
	path    string
	covered bool
}

// contribute is the M7 contributing stage: it reconciles the validated sidecars'
// placeholder work slug to the real meta.audiosilo.app work and publishes them to
// KodeStar/audiosilo-meta per the configured mode (issue / pr / local), then writes
// its sentinel. The flow mirrors M7-DESIGN.md ("The contributing stage") step by step.
//
// Parks (all Retry-re-admittable) replace a hard failure for a human-fixable
// precondition: no GitHub credential (issue/pr) parks ParkContribUnavailable; a work
// that does not exist upstream parks ParkCoreNeeded (after prefilling a proposal) or
// ParkCorePending (a proposal already in flight). A GitHub rate limit returns a plain
// (transient) stage error rather than a park.
func (e *Executor) contribute(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	start := time.Now()

	// 1. Load which sidecars exist. At least one must be present (synthesis normally
	// guarantees both) - otherwise there is nothing to contribute, a hard failure.
	charsPath := filepath.Join(book.WorkDir, sidecarsDir, charactersFileName)
	recapsPath := filepath.Join(book.WorkDir, sidecarsDir, recapsFileName)
	haveChars := fsutil.IsFile(charsPath)
	haveRecaps := fsutil.IsFile(recapsPath)
	if !haveChars && !haveRecaps {
		return scheduler.StageResult{}, fmt.Errorf("contributing: no sidecars found under %s/ (synthesizing must run first)", sidecarsDir)
	}

	// 2. Resolve the real work slug (verify a manual match, else asin/isbn lookup, else
	// needs-core / local placeholder). A transient error propagates as a stage error; a
	// park propagates as a ParkError.
	slug, placeholderNote, park, err := e.resolveWorkSlug(ctx, book)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if park != nil {
		return scheduler.StageResult{}, park
	}
	if !model.ValidSlug(slug) {
		return scheduler.StageResult{}, fmt.Errorf("contributing: resolved work slug %q is not a valid slug", slug)
	}

	// 3. Reconcile the durable sidecar files: rewrite "work" to the resolved slug and
	// stamp the source audiobook edition, canonicalized in place, so what ships can be
	// audited against the recording used for extraction.
	sourceRef := e.contributionSourceRef(ctx, book, slug)
	var artifacts []contribArtifact
	if haveChars {
		if err := reconcileSidecar(charsPath, slug, sourceRef); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("contributing: reconcile %s: %w", charactersFileName, err)
		}
		artifacts = append(artifacts, contribArtifact{kind: store.ContribKindCharacters, path: charsPath})
	}
	if haveRecaps {
		if err := reconcileSidecar(recapsPath, slug, sourceRef); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("contributing: reconcile %s: %w", recapsFileName, err)
		}
		artifacts = append(artifacts, contribArtifact{kind: store.ContribKindRecaps, path: recapsPath})
	}

	// 4. Skip-if-covered: a dimension the resolved work already carries upstream is
	// recorded already_covered and never submitted. ErrWorkNotFound/transport/disabled
	// degrade to "not covered" (metaissue's overwrite refusal is the backstop).
	hasChars, hasRecaps := e.coveredDimensions(ctx, slug)
	for i := range artifacts {
		if artifacts[i].kind == store.ContribKindCharacters {
			artifacts[i].covered = hasChars
		} else {
			artifacts[i].covered = hasRecaps
		}
	}

	// 5. Submit per mode (idempotent on resume). A book whose sidecars were accepted on a
	// converging audit trajectory (rather than a clean zero-finding pass) carries a note
	// so the residual-nits fact rides to the UI; the note is process metadata only and
	// NEVER enters the public issue/PR payload (metaissue parses the body).
	auditNote := auditAcceptanceNote(book.WorkDir)
	if err := e.submitContributions(ctx, book, slug, artifacts, placeholderNote, auditNote); err != nil {
		return scheduler.StageResult{}, err
	}

	// 6. Sentinel + a whole-book rate sample.
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	submitted, covered := 0, 0
	for _, a := range artifacts {
		if a.covered {
			covered++
		} else {
			submitted++
		}
	}
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"mode":      e.contribModeOrDefault(),
			"work":      slug,
			"submitted": submitted,
			"covered":   covered,
		}),
		RateSample: rateSample(1, time.Since(start).Seconds()),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Contributing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// resolveWorkSlug determines the upstream work slug for the book's sidecars. It
// returns (slug, placeholderNote, park, err): park is a non-nil ParkError when the
// book must wait for a human/poller (needs-core in issue/pr mode); err is a transient
// stage error (a metadata transport failure while verifying a manual match).
// placeholderNote is set only when local mode proceeds on a title-derived placeholder
// (no upstream match), so the recorded rows carry that provenance.
func (e *Executor) resolveWorkSlug(ctx context.Context, book store.Book) (slug, placeholderNote string, park, err error) {
	// (a) A manual/enqueue-time match: verify it still resolves upstream. A clean 404
	// is a stale match (fall through to lookup); a disabled service can't verify, so
	// trust the recorded match; any other error is transient.
	if id := strings.TrimSpace(book.WorkID); id != "" {
		_, verr := e.metaCoverageForWork(ctx, id)
		switch {
		case verr == nil:
			return id, "", nil, nil
		case errors.Is(verr, metaops.ErrWorkNotFound):
			// A 404 normally means a stale manual match - fall through to identifier
			// lookup. BUT when this book's core add-work row is merged, book.WorkID came
			// from that merged intake PR's files; the upstream data release just has not
			// rebuilt to include the new work yet, so trust the slug rather than
			// re-parking needs-core (which would loop forever).
			if e.hasMergedCoreRow(ctx, book.ID) {
				return id, "", nil, nil
			}
			// stale manual match - fall through to identifier lookup
		case errors.Is(verr, metaops.ErrDisabled):
			return id, "", nil, nil
		default:
			return "", "", nil, fmt.Errorf("contributing: verify work %q: %w", id, verr)
		}
	}

	// (b) Exact-identifier lookup (ASIN then ISBN). NEVER a fuzzy title search here:
	// the identity is passed without a title so metaops cannot auto-adopt a fuzzy
	// match (wrong-work attachment is a spoiler hazard - the human paths own that).
	if wid, ok := e.lookupByIdentifier(ctx, book); ok {
		if e.db != nil {
			_ = e.db.SetBookWorkID(context.WithoutCancel(ctx), book.ID, wid)
		}
		return wid, "", nil, nil
	}

	// (c) No upstream match. Local mode proceeds on a placeholder; issue/pr modes need
	// a core add-work proposal first.
	if e.contribModeOrDefault() == contribModeLocal {
		return ExportSlug(book), placeholderExportNote, nil, nil
	}
	park, err = e.needsCore(ctx, book)
	return "", "", park, err
}

// hasMergedCoreRow reports whether the book has a kind=core contribution row in the
// merged state (its add-work intake PR merged). When it does, book.WorkID was resolved
// from that PR's files and is trustworthy even before the upstream data release rebuilds
// to include the new work - so a CoverageForWork 404 must not re-park the book.
func (e *Executor) hasMergedCoreRow(ctx context.Context, bookID int64) bool {
	for _, r := range e.bookContributions(ctx, bookID) {
		if r.Kind == store.ContribKindCore && r.Status == store.ContribStatusMerged {
			return true
		}
	}
	return false
}

// metaCoverageForWork verifies/reads a work upstream, treating a nil meta client as a
// disabled service.
func (e *Executor) metaCoverageForWork(ctx context.Context, workID string) (metaops.Coverage, error) {
	if e.meta == nil {
		return metaops.Coverage{}, metaops.ErrDisabled
	}
	return e.meta.CoverageForWork(ctx, workID)
}

// lookupByIdentifier resolves the book's ASIN/ISBN to a work id, accepting ONLY an
// exact identifier match (matched_by asin|isbn) - a fuzzy "search" verdict is
// rejected. It returns ("", false) when the metadata service is disabled/unreachable,
// there is no identifier, or nothing matched.
func (e *Executor) lookupByIdentifier(ctx context.Context, book store.Book) (string, bool) {
	if e.meta == nil {
		return "", false
	}
	asin := strings.TrimSpace(book.ASIN)
	isbn := strings.TrimSpace(book.ISBN)
	if asin == "" && isbn == "" {
		return "", false
	}
	// No Title: metaops.CoverageFor skips its fuzzy title-search step, so the verdict
	// can only be an exact asin/isbn match or a clean miss.
	cov, err := e.meta.CoverageFor(ctx, metaops.BookIdentity{ASIN: asin, ISBN: isbn})
	if err != nil {
		return "", false
	}
	if cov.Known && cov.WorkID != "" && (cov.MatchedBy == "asin" || cov.MatchedBy == "isbn") {
		return cov.WorkID, true
	}
	return "", false
}

// needsCore handles an issue/pr book whose work does not exist upstream. When a core
// proposal is already in flight (a core contribution row submitted/pr_open/merged) it
// parks ParkCorePending (the poller resolves the slug and re-admits). Otherwise it
// prefills contrib/core_proposal.json and parks ParkCoreNeeded.
func (e *Executor) needsCore(ctx context.Context, book store.Book) (park, err error) {
	if e.db != nil {
		rows, rerr := e.db.ListContributionsByBook(ctx, book.ID)
		if rerr != nil {
			return nil, fmt.Errorf("contributing: list contributions: %w", rerr)
		}
		for _, r := range rows {
			if r.Kind != store.ContribKindCore {
				continue
			}
			switch r.Status {
			case store.ContribStatusSubmitted, store.ContribStatusPROpen, store.ContribStatusMerged:
				// A proposal is in flight (merged-but-work_id-empty is the poller race:
				// the asin/isbn lookup above was the one re-check; keep waiting).
				return scheduler.ParkWithCode(state.ParkCorePending, CorePendingMsg), nil
			}
		}
	}
	if err := e.writeCoreProposal(book); err != nil {
		return nil, fmt.Errorf("contributing: write core proposal: %w", err)
	}
	return scheduler.ParkWithCode(state.ParkCoreNeeded, CoreNeededMsg), nil
}

// writeCoreProposal prefills contrib/core_proposal.json from the book row for the UI
// to complete: the factual fields the scan knows (title/authors/narrators/series/
// asin/isbn), the ASR-detected language, the manifest runtime in minutes, and an
// honest provenance line. Region is left blank (unknown - the UI defaults it to "us");
// everything unknown is omitted (never guessed).
func (e *Executor) writeCoreProposal(book store.Book) error {
	p := contrib.CoreProposal{
		Title:          book.Title,
		Authors:        book.Authors,
		Narrators:      book.Narrators,
		SeriesName:     book.Series,
		SeriesPosition: book.SeriesPos,
		Language:       readASRProvenance(book.WorkDir).Language,
		RuntimeMin:     manifestRuntimeMin(book.WorkDir),
		Sources:        coreProvenanceLine,
	}
	if asin := strings.TrimSpace(book.ASIN); asin != "" {
		p.ASINs = []contrib.RegionASIN{{ASIN: asin}} // Region left for the user
	}
	if isbn := strings.TrimSpace(book.ISBN); isbn != "" {
		p.AudiobookISBNs = []string{isbn}
	}
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// WriteFileAtomic MkdirAlls the parent, so no explicit MkdirAll is needed here.
	path := filepath.Join(book.WorkDir, contribDir, coreProposalName)
	return fsutil.WriteFileAtomic(path, append(out, '\n'), 0o644)
}

// coveredDimensions reports which sidecar dimensions the resolved work already carries
// upstream. A nil/disabled meta client, a not-found work, or a transport error all
// degrade to "not covered" so the stage proceeds (metaissue's overwrite refusal is the
// upstream backstop).
func (e *Executor) coveredDimensions(ctx context.Context, slug string) (hasChars, hasRecaps bool) {
	if e.meta == nil {
		return false, false
	}
	cov, err := e.meta.CoverageForWork(ctx, slug)
	if err != nil {
		return false, false
	}
	return cov.HasCharacters, cov.HasRecaps
}

// submitContributions dispatches to the configured mode's submit path. auditNote (a
// non-empty "audit converged..." line when the sidecars were accepted on a converging
// trajectory) is appended to each submitted row's note.
func (e *Executor) submitContributions(ctx context.Context, book store.Book, slug string, artifacts []contribArtifact, placeholderNote, auditNote string) error {
	switch e.contribModeOrDefault() {
	case contribModeLocal:
		return e.submitLocal(ctx, book, slug, artifacts, placeholderNote, auditNote)
	case contribModePR:
		return e.submitPR(ctx, book, slug, artifacts, auditNote)
	default:
		return e.submitIssue(ctx, book, slug, artifacts, auditNote)
	}
}

// joinNotes concatenates the non-empty note parts with "; " (a row note is free text
// that rides to the UI). It drops empty parts so a row with no per-row note carries just
// the audit note, and vice versa.
func joinNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

// submitIssue opens one intake issue per uncovered dimension. It verifies the routing
// label stuck (GitHub silently drops labels from non-collaborators) and notes when it
// did not, and switches an oversize body to a secret gist link. A RateLimitError
// propagates as a transient stage error (not a park); a resume with an already-recorded
// row skips that dimension.
func (e *Executor) submitIssue(ctx context.Context, book store.Book, slug string, artifacts []contribArtifact, auditNote string) error {
	cli, err := e.contribClient(ctx)
	if err != nil {
		return err
	}
	existing := e.bookContributions(ctx, book.ID)
	for _, a := range artifacts {
		// A settled row (submitted/merged/local/already_covered) is authoritative: skip
		// it BEFORE the covered branch so a later upstream-covered verdict cannot
		// overwrite a row that already recorded a real submission on a prior run.
		if rowSettled(existing, a.kind) {
			continue // resume: this dimension already posted
		}
		if a.covered {
			if err := e.recordCovered(ctx, book.ID, a.kind, store.ContribModeIssue); err != nil {
				return err
			}
			continue
		}
		payload, rerr := os.ReadFile(a.path) //nolint:gosec // path derives from the book's work dir
		if rerr != nil {
			return rerr
		}
		title, body, labels := composeIssue(a.kind, slug, payload, "")
		if contrib.ExceedsBodyLimit(body) {
			file := fileNameFor(a.kind)
			raws, gerr := cli.CreateGist(ctx, map[string]string{file: string(payload)}, true)
			if gerr != nil {
				return gerr
			}
			title, body, labels = composeIssue(a.kind, slug, payload, raws[file])
		}
		issue, ierr := cli.CreateIssue(ctx, e.contribRepo, title, body, labels)
		if ierr != nil {
			return ierr
		}
		note := ""
		if got, gerr := cli.GetIssue(ctx, e.contribRepo, issue.Number); gerr == nil {
			if !labelStuck(got.Labels, routingLabel(a.kind)) {
				note = fmt.Sprintf("%s - a maintainer must apply %s for intake to run", store.ContribNoteLabelsMissingPrefix, routingLabel(a.kind))
			}
		}
		if err := e.upsertRow(ctx, book.ID, a.kind, store.ContribModeIssue, issue.Number, issue.URL, store.ContribStatusSubmitted, joinNotes(note, auditNote)); err != nil {
			return err
		}
	}
	return nil
}

// submitPR forks the meta repo, branches sidecars/<slug>-<bookID>, commits each
// uncovered sidecar at its canonical data/works/<shard>/<slug>/ path, and opens ONE
// pull request that both dimension rows share. Covered dimensions are recorded
// already_covered; a resume where every uncovered dimension already has a URL is a
// no-op.
func (e *Executor) submitPR(ctx context.Context, book store.Book, slug string, artifacts []contribArtifact, auditNote string) error {
	cli, err := e.contribClient(ctx)
	if err != nil {
		return err
	}
	existing := e.bookContributions(ctx, book.ID)
	var toSubmit []contribArtifact
	for _, a := range artifacts {
		// Settled first, so an upstream-covered verdict never overwrites a prior submission.
		if rowSettled(existing, a.kind) {
			continue
		}
		if a.covered {
			if err := e.recordCovered(ctx, book.ID, a.kind, store.ContribModePR); err != nil {
				return err
			}
			continue
		}
		toSubmit = append(toSubmit, a)
	}
	if len(toSubmit) == 0 {
		return nil
	}

	fork, err := cli.EnsureFork(ctx, e.contribRepo)
	if err != nil {
		return err
	}
	branch := fmt.Sprintf("sidecars/%s-%d", slug, book.ID)
	// Crash-resume idempotency: a prior run may have created the branch (and even the
	// files/PR) but persisted no rows. Reuse an existing branch instead of re-creating it
	// (CreateRef 422s "reference already exists").
	_, branchExists, berr := cli.BranchRef(ctx, fork, branch)
	if berr != nil {
		return berr
	}
	if !branchExists {
		_ = cli.MergeUpstream(ctx, fork, "main") // best-effort fast-forward
		sha, serr := cli.BranchSHA(ctx, fork, "main")
		if serr != nil {
			return serr
		}
		if err := cli.CreateRef(ctx, fork, "refs/heads/"+branch, sha); err != nil {
			return err
		}
	}
	shard := model.Shard(slug)
	for _, a := range toSubmit {
		content, rerr := os.ReadFile(a.path) //nolint:gosec // path derives from the book's work dir
		if rerr != nil {
			return rerr
		}
		file := fileNameFor(a.kind)
		path := fmt.Sprintf("data/works/%s/%s/%s", shard, slug, file)
		// PutContents supplies the existing blob sha when the file is already on the branch
		// (a resumed run), so a re-commit updates rather than 422ing on a create.
		if err := cli.PutContents(ctx, fork, branch, path, "Add "+file+" for "+slug, content); err != nil {
			return err
		}
	}
	head := contrib.OwnerOf(fork) + ":" + branch
	// Reuse an already-open PR for this head (a prior run opened it) instead of 422ing.
	pr, prExists, ferr := cli.FindOpenPRByHead(ctx, e.contribRepo, head)
	if ferr != nil {
		return ferr
	}
	if !prExists {
		pr, err = cli.CreatePull(ctx, e.contribRepo, head, "main", "Add sidecars for "+slug, prBody(book, slug, toSubmit))
		if err != nil {
			return err
		}
	}
	for _, a := range toSubmit {
		if err := e.upsertRow(ctx, book.ID, a.kind, store.ContribModePR, pr.Number, pr.URL, store.ContribStatusSubmitted, auditNote); err != nil {
			return err
		}
	}
	return nil
}

// submitLocal exports each uncovered sidecar into <exportRoot>/works/<shard>/<slug>/
// in repo layout, recording a local row (with placeholderNote when the slug is a
// title-derived placeholder). No network, no credential.
func (e *Executor) submitLocal(ctx context.Context, book store.Book, slug string, artifacts []contribArtifact, placeholderNote, auditNote string) error {
	shard := model.Shard(slug)
	// WriteFileAtomic MkdirAlls the destination parent, so no explicit MkdirAll here.
	destDir := filepath.Join(e.exportRoot, "works", shard, slug)
	existing := e.bookContributions(ctx, book.ID)
	for _, a := range artifacts {
		// Settled first: a settled local/covered row from a prior run stays authoritative
		// (a later covered verdict must not overwrite a recorded local export).
		if rowSettled(existing, a.kind) {
			continue
		}
		if a.covered {
			if err := e.recordCovered(ctx, book.ID, a.kind, store.ContribModeLocal); err != nil {
				return err
			}
			continue
		}
		content, rerr := os.ReadFile(a.path) //nolint:gosec // path derives from the book's work dir
		if rerr != nil {
			return rerr
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(destDir, fileNameFor(a.kind)), content, 0o644); err != nil {
			return err
		}
		if err := e.upsertRow(ctx, book.ID, a.kind, store.ContribModeLocal, 0, "", store.ContribStatusLocal, joinNotes(placeholderNote, auditNote)); err != nil {
			return err
		}
	}
	return nil
}

// contribClient resolves a GitHub credential and builds a REST client. A missing
// credential is a ParkError (ParkContribUnavailable); any other resolve failure is a
// transient stage error.
func (e *Executor) contribClient(ctx context.Context) (*contrib.Client, error) {
	if e.tokenSource == nil {
		return nil, scheduler.ParkWithCode(state.ParkContribUnavailable, ContribUnavailableMsg)
	}
	token, _, err := e.tokenSource.Resolve(ctx)
	if err != nil {
		if errors.Is(err, contrib.ErrNoCredential) {
			return nil, scheduler.ParkWithCode(state.ParkContribUnavailable, ContribUnavailableMsg)
		}
		return nil, fmt.Errorf("contributing: resolve credential: %w", err)
	}
	base := e.contribBaseURL
	if base == "" {
		base = defaultContribBaseURL
	}
	return contrib.NewClient(base, token), nil
}

// recordCovered records a dimension the resolved work already carries upstream as an
// already_covered row (never submitted), for the given contribution mode. The three
// submit paths share it.
func (e *Executor) recordCovered(ctx context.Context, bookID int64, kind, mode string) error {
	return e.upsertRow(ctx, bookID, kind, mode, 0, "", store.ContribStatusAlreadyCovered, "")
}

// bookContributions reads a book's existing contribution rows once (so a submit path
// need not re-query per dimension). A nil db or a read error yields no rows - the
// caller then treats every dimension as not-yet-settled and (re)submits, which is
// idempotent on (book, kind).
func (e *Executor) bookContributions(ctx context.Context, bookID int64) []store.Contribution {
	if e.db == nil {
		return nil
	}
	rows, err := e.db.ListContributionsByBook(ctx, bookID)
	if err != nil {
		return nil
	}
	return rows
}

// upsertRow records/updates a contribution row (idempotent on book+kind).
func (e *Executor) upsertRow(ctx context.Context, bookID int64, kind, mode string, number int, url, status, note string) error {
	if e.db == nil {
		return nil
	}
	_, err := e.db.UpsertContribution(context.WithoutCancel(ctx), store.Contribution{
		BookID: bookID, Kind: kind, Mode: mode, Repo: e.contribRepo,
		Number: number, URL: url, Status: status, Note: note,
	})
	if err != nil {
		return fmt.Errorf("contributing: record %s contribution: %w", kind, err)
	}
	return nil
}

// rowSettled reports whether a dimension's contribution (in the pre-fetched rows) is
// already recorded as done (has a URL, or is a terminal local/already_covered row), so
// a resume after a crash between submit and sentinel does not double-post. Pure over the
// caller-fetched rows (see bookContributions).
func rowSettled(rows []store.Contribution, kind string) bool {
	for _, r := range rows {
		if r.Kind != kind {
			continue
		}
		return r.URL != "" || r.Status == store.ContribStatusLocal || r.Status == store.ContribStatusAlreadyCovered
	}
	return false
}

// contribModeOrDefault returns the configured mode, defaulting an empty value to issue.
func (e *Executor) contribModeOrDefault() string {
	if e.contribMode == "" {
		return contribModeIssue
	}
	return e.contribMode
}

// reconcileSidecar rewrites a sidecar file's "work" to slug, replaces the
// staging-only bare community source with the source audiobook edition, and
// canonicalizes it in place (atomic write).
func reconcileSidecar(path, slug, sourceRef string) error {
	if filepath.Base(path) == charactersFileName {
		var c model.Characters
		if err := decodeSidecarFile(path, &c); err != nil {
			return err
		}
		c.Work = slug
		c.Sources = contributionSources(sourceRef)
		return writeCanonicalJSON(path, c)
	}
	var r model.Recaps
	if err := decodeSidecarFile(path, &r); err != nil {
		return err
	}
	r.Work = slug
	r.Sources = contributionSources(sourceRef)
	return writeCanonicalJSON(path, r)
}

// contributionSources returns the one community provenance entry permitted by the
// sidecar contract. The ref names the actual audiobook edition, not this tool.
func contributionSources(sourceRef string) []model.Source {
	return []model.Source{{Type: sourceTypeCommunity, Ref: sourceRef}}
}

// contributionSourceRef identifies the audiobook edition used for extraction. A
// locally captured ASIN/ISBN is strongest. Otherwise the resolved work's recordings
// are matched against local narrator/runtime evidence; only a tie-free match is used.
func (e *Executor) contributionSourceRef(ctx context.Context, book store.Book, slug string) string {
	cov, covErr := e.metaCoverageForWork(ctx, slug)
	if asin := strings.TrimSpace(book.ASIN); asin != "" {
		for _, rec := range cov.Recordings {
			for _, candidate := range rec.ASINs {
				if strings.EqualFold(strings.TrimSpace(candidate.ASIN), asin) && strings.TrimSpace(candidate.Region) != "" {
					return "audible:" + strings.ToLower(strings.TrimSpace(candidate.Region)) + ":" + asin
				}
			}
		}
		return "audible:" + asin
	}
	if isbn := strings.TrimSpace(book.ISBN); isbn != "" {
		return "isbn:" + isbn
	}
	if covErr == nil {
		if rec := bestRecordingRef(book, cov.Recordings); rec != nil {
			for _, region := range []string{"us", "uk", "au", "ca"} {
				for _, asin := range rec.ASINs {
					if strings.EqualFold(asin.Region, region) && strings.TrimSpace(asin.ASIN) != "" {
						return "audible:" + strings.ToLower(asin.Region) + ":" + strings.TrimSpace(asin.ASIN)
					}
				}
			}
			if len(rec.ASINs) > 0 && strings.TrimSpace(rec.ASINs[0].ASIN) != "" {
				return "audible:" + strings.TrimSpace(rec.ASINs[0].ASIN)
			}
			if len(rec.ISBNs) > 0 && strings.TrimSpace(rec.ISBNs[0]) != "" {
				return "isbn:" + strings.TrimSpace(rec.ISBNs[0])
			}
			if rec.ID != "" {
				return "audiosilo-meta:recording:" + slug + "/" + rec.ID
			}
		}
	}
	return "audiosilo-meta:work:" + slug
}

func bestRecordingRef(book store.Book, recordings []metaops.RecordingRef) *metaops.RecordingRef {
	if len(recordings) == 1 {
		return &recordings[0]
	}
	wantNarrators := make(map[string]bool, len(book.Narrators))
	for _, narrator := range book.Narrators {
		wantNarrators[normaliseName(narrator)] = true
	}
	best, bestScore := -1, -1
	for i := range recordings {
		score := 0
		for _, narrator := range recordings[i].Narrators {
			if wantNarrators[normaliseName(narrator)] {
				score += 10
			}
		}
		if book.DurationSec > 0 && recordings[i].RuntimeMin > 0 {
			delta := math.Abs(book.DurationSec/60 - float64(recordings[i].RuntimeMin))
			if delta <= math.Max(5, float64(recordings[i].RuntimeMin)*0.02) {
				score += 5
			}
		}
		if score > bestScore {
			best, bestScore = i, score
		} else if score == bestScore {
			best = -1
		}
	}
	if best < 0 || bestScore <= 0 {
		return nil
	}
	return &recordings[best]
}

func normaliseName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeCanonicalJSON marshals v, canonicalizes it (metafmt-equivalent), and atomically
// writes it to path.
func writeCanonicalJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	formatted, err := canonical.Format(raw)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, formatted, 0o644)
}

// manifestRuntimeMin returns the book's total runtime in minutes (rounded) from the
// manifest, or 0 when the manifest is unreadable.
func manifestRuntimeMin(workDir string) int {
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return 0
	}
	return int(math.Round(m.Duration / 60.0))
}

// composeIssue composes the intake issue title/body/labels for a dimension.
func composeIssue(kind, slug string, payload []byte, gistURL string) (title, body string, labels []string) {
	if kind == store.ContribKindRecaps {
		return contrib.RecapsIssue(slug, payload, gistURL)
	}
	return contrib.CharactersIssue(slug, payload, gistURL)
}

// fileNameFor maps a contribution kind to its sidecar file name.
func fileNameFor(kind string) string {
	if kind == store.ContribKindRecaps {
		return recapsFileName
	}
	return charactersFileName
}

// routingLabel maps a contribution kind to the intake routing label the meta repo's
// workflow gates on.
func routingLabel(kind string) string {
	if kind == store.ContribKindRecaps {
		return "data:recaps"
	}
	return "data:characters"
}

// labelStuck reports whether want is among the labels GitHub echoed back.
func labelStuck(got []string, want string) bool {
	return slices.Contains(got, want)
}

// prBody composes the PR description (the files, the CC BY-SA statement, the book
// title). No commit/PR trailers (workspace convention).
func prBody(book store.Book, slug string, artifacts []contribArtifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Community sidecars for **%s** (`%s`).\n\n", book.Title, slug)
	b.WriteString("Files:\n")
	for _, a := range artifacts {
		fmt.Fprintf(&b, "- `data/works/%s/%s/%s`\n", model.Shard(slug), slug, fileNameFor(a.kind))
	}
	b.WriteString("\nLicensed under CC BY-SA 3.0. Own-words, spoiler-gated; generated by audiosilo-sidecars.\n")
	return b.String()
}

// placeholderExportNote annotates a local-export row whose work could not be resolved
// upstream (exported under a title-derived placeholder slug).
const placeholderExportNote = "work not found upstream - exported under a placeholder slug; set the work before contributing"
