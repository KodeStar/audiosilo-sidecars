package pipeline

import (
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
)

// fullFakeOpts configures fullFakeAct - the composite fake agent that scripts a
// minimally-valid output for EVERY agent stage, so a book can be driven all the way to
// done through the real scheduler with no live CLI. Every field has a sensible default,
// and the per-stage behaviors that loop tests need (adjudication disposition, audit fail
// rounds) are overridable.
type fullFakeOpts struct {
	// title is the book title; spelling_research's spellings.json Title must equal it.
	title string
	// workSlug is the sidecar work slug the synthesis/fix outputs carry (default "book").
	workSlug string
	// refs is the spelling ledger's reference_files (default {"marker_titles.txt"}).
	refs []string
	// adjudicate overrides the qa_adjudicating disposition per chapter (default: every
	// flagged chapter is "accept"). A chapter mapped to retranscribe/tail_clip drives the
	// retranscribing leg.
	adjudicate map[int]qa.PlanAction
	// adjReason is the reason text on every plan entry (default "adjudicated by fake").
	adjReason string
	// auditFail is the number of LEADING auditing rounds that return a BLOCKER before the
	// auditor passes (default 0 = pass on the first round). Drives the audit->fix->audit
	// loop.
	auditFail int
}

// fullFakeAct returns a fakeRunner act func that writes a minimally-valid output for
// every agent stage, dispatching on req.Stage. It composes the per-stage act helpers the
// focused stage tests already use (validSpellingAct, factPassAct, writeOutSidecars) and
// adds the markers/adjudicate/audit scripts, so one runner drives a whole book to done.
//
// The `attempt` argument is the fakeRunner's 1-based per-stage invocation count, which for
// auditing (whose output is always structurally valid, so it never triggers a
// validator-retry) equals the round number - used to fail the first opts.auditFail rounds.
func fullFakeAct(t *testing.T, opts fullFakeOpts) func(*fakeRunner, agent.Request, int) (agent.Result, error) {
	t.Helper()
	if opts.workSlug == "" {
		opts.workSlug = "book"
	}
	if opts.refs == nil {
		opts.refs = []string{markerTitlesFile}
	}
	if opts.adjReason == "" {
		opts.adjReason = "adjudicated by fake"
	}
	spellingAct := validSpellingAct(t, opts.title, opts.refs)
	factAct := factPassAct(t)

	return func(f *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		switch req.Stage {
		case string(state.MarkersNormalizing):
			return fakeMarkersAct(t, req)
		case string(state.QAAdjudicating):
			return fakeAdjudicateAct(t, req, opts)
		case string(state.SpellingResearch):
			return spellingAct(f, req, attempt)
		case string(state.FactPass):
			return factAct(f, req, attempt)
		case string(state.Synthesizing), string(state.Fixing):
			writeOutSidecars(t, req, opts.workSlug)
			return agent.Result{Usage: agent.Usage{Model: "opus", Input: 220, Output: 110, CostUSD: 0.12, Turns: 2}}, nil
		case string(state.Auditing):
			return fakeAuditAct(t, req, attempt, opts.auditFail)
		default:
			return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 10, Output: 5}}, nil
		}
	}
}

// fakeMarkersAct reads the staged draft manifest, renumbers its chapters contiguously
// (keeping every interval and file path so the validator's subset/interval checks pass),
// and writes out/manifest.json + a confident out/verdict.json.
func fakeMarkersAct(t *testing.T, req agent.Request) (agent.Result, error) {
	t.Helper()
	draft, err := audio.ReadManifest(req.Dir)
	if err != nil {
		t.Fatalf("fake markers: read staged manifest: %v", err)
	}
	corrected := draft
	corrected.Chapters = make([]audio.Chapter, len(draft.Chapters))
	for i, ch := range draft.Chapters {
		ch.Chapter = i + 1
		corrected.Chapters[i] = ch
	}
	corrected.ChapterCount = len(corrected.Chapters)
	writeOut(t, req, audio.ManifestName, corrected)
	writeOut(t, req, "verdict.json", markerVerdict{Confident: true, Reason: "renumbered contiguously"})
	return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 130, Output: 65, CostUSD: 0.02, Turns: 2}}, nil
}

// fakeAdjudicateAct loads the staged qa_report.json, then dispositions exactly the
// flagged chapters (the set the plan validator requires), using opts.adjudicate for a
// chapter's action (default "accept").
func fakeAdjudicateAct(t *testing.T, req agent.Request, opts fullFakeOpts) (agent.Result, error) {
	t.Helper()
	rep, err := qa.LoadReport(req.Dir)
	if err != nil {
		t.Fatalf("fake adjudicate: load staged qa report: %v", err)
	}
	var entries []qa.PlanEntry
	for _, ch := range qa.FlaggedChapters(rep) {
		action := qa.ActionAccept
		if a, ok := opts.adjudicate[ch]; ok {
			action = a
		}
		entries = append(entries, qa.PlanEntry{Chapter: ch, Action: action, Reason: opts.adjReason})
	}
	writeOut(t, req, qa.PlanFile, qa.Plan{Entries: entries})
	return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 90, Output: 45, CostUSD: 0.02}}, nil
}

// fakeAuditAct passes the audit unless this is one of the first `fail` rounds, in which
// case it returns a single BLOCKER (driving another fix round).
func fakeAuditAct(t *testing.T, req agent.Request, attempt, fail int) (agent.Result, error) {
	t.Helper()
	if attempt <= fail {
		writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
			{Severity: SeverityBlocker, Locus: "characters[0].description", Text: "leak", Evidence: "ch3", Suggestion: "trim"},
		}})
	} else {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
	}
	return agent.Result{Usage: agent.Usage{Model: "opus", Input: 140, Output: 70, CostUSD: 0.09}}, nil
}

// fullFakeConfig builds a Config wiring the fake agent runner (available) with a stub
// fallback (so the ready/contributing waypoints advance), for a to-done drive.
func fullFakeConfig(dataDir string, fake *fakeRunner) Config {
	return Config{
		DataDir:    dataDir,
		Agent:      fake,
		AgentAvail: agent.Availability{Backend: agent.IDClaude, Available: true, Version: "fake"},
		Fallback:   scheduler.NewStubExecutor(0, 0),
	}
}
