package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

// --- pure decision logic ---

func TestAcceptTrajectory(t *testing.T) {
	const maxFix = state.MaxFixAttempts // 3
	cases := []struct {
		name                string
		round, blocker, fix int
		prevFix             int
		prevOK              bool
		fixesDone           int
		valClean            bool
		want                bool
	}{
		// The converging book-3 case: round 2, fix 1 <= prev 4, budget left -> accept.
		{"converging", 2, 0, 1, 4, true, 1, true, true},
		{"flat-trajectory", 3, 0, 2, 2, true, 2, true, true},
		{"blocker-never-accepts", 2, 1, 1, 4, true, 1, true, false},
		{"unclean-validation-never-accepts", 2, 0, 1, 4, true, 1, false, false},
		{"first-round-too-early", 1, 0, 1, 0, false, 0, true, false},
		{"fix-zero-is-a-pass-not-accept", 2, 0, 0, 1, true, 1, true, false},
		{"fix-over-cap", 2, 0, 3, 4, true, 1, true, false},
		{"growing-trajectory", 2, 0, 2, 1, true, 1, true, false},
		{"budget-exhausted", 4, 0, 1, 1, true, maxFix, true, false},
		{"no-previous-round", 2, 0, 1, 0, false, 1, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := acceptTrajectory(c.round, c.blocker, c.fix, c.prevFix, c.prevOK, c.fixesDone, c.valClean, maxFix)
			if got != c.want {
				t.Errorf("acceptTrajectory = %v, want %v", got, c.want)
			}
		})
	}
}

// --- direct-Execute stage behaviour (db-backed) ---

// auditBook creates a db-backed book seeded with the audit prerequisites (manifest,
// facts, sidecars, and a validation report with the given clean flag).
func auditBook(t *testing.T, db *store.DB, valClean bool) store.Book {
	t.Helper()
	work := t.TempDir()
	seedForAudit(t, work, valClean)
	b, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: "/src/audit", WorkDir: work, Title: "Book"})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	return b
}

// recordAuditSuccess records one completed auditing run so CountStageSuccesses(auditing)
// reflects a prior round (the re-entry paths need done >= 1).
func recordAuditSuccess(t *testing.T, db *store.DB, bookID int64, attempt int) {
	t.Helper()
	id, err := db.StartStageRun(context.Background(), bookID, string(state.Auditing), attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishStageRun(context.Background(), id, true, nil); err != nil {
		t.Fatal(err)
	}
}

// TestAuditReentryPassesWithoutAgent: an acceptance marker plus a clean validation report
// on re-entry passes the stage WITHOUT invoking the agent, carrying the accepted metrics.
func TestAuditReentryPassesWithoutAgent(t *testing.T) {
	db := openContribDB(t)
	b := auditBook(t, db, true) // clean validation
	recordAuditSuccess(t, db, b.ID, 1)
	if err := writeAuditAccepted(b.WorkDir, auditAccepted{
		Round: 2, Fix: 1, Nit: 2,
		Findings: []AuditFinding{{Severity: SeverityFix, Locus: "x"}, {Severity: SeverityNit, Locus: "y"}},
	}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, _ agent.Request, _ int) (agent.Result, error) {
		t.Error("agent invoked on an accepted re-entry; the pass must be agentless")
		return agent.Result{}, nil
	}
	cfg := withSidecarAgent(t.TempDir(), fake)
	cfg.DB = db
	res, err := NewExecutor(cfg).Execute(context.Background(), b, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if !res.AuditPassed {
		t.Error("AuditPassed = false, want true (accepted re-entry)")
	}
	if n := fake.count(string(state.Auditing)); n != 0 {
		t.Errorf("agent invoked %d times, want 0", n)
	}
	var m struct {
		AcceptedAfterRounds int `json:"accepted_after_rounds"`
		ResidualNits        int `json:"residual_nits"`
	}
	if err := json.Unmarshal(res.Metrics, &m); err != nil {
		t.Fatal(err)
	}
	if m.AcceptedAfterRounds != 2 || m.ResidualNits != 2 {
		t.Errorf("metrics accepted_after_rounds=%d residual_nits=%d, want 2/2", m.AcceptedAfterRounds, m.ResidualNits)
	}
	if !scheduler.SentinelExists(b.WorkDir, string(state.Auditing)) {
		t.Error("auditing sentinel missing after the agentless pass")
	}
}

// TestAuditReentryValidationDirtyRunsRealRound: a marker whose final fix left validation
// UNCLEAN drops the marker and runs a real (agent) audit round.
func TestAuditReentryValidationDirtyRunsRealRound(t *testing.T) {
	db := openContribDB(t)
	b := auditBook(t, db, false) // validation NOT clean
	recordAuditSuccess(t, db, b.ID, 1)
	if err := writeAuditAccepted(b.WorkDir, auditAccepted{Round: 2, Fix: 1, Nit: 1}); err != nil {
		t.Fatal(err)
	}
	// Open the current run so agent usage recording has a target.
	if _, err := db.StartStageRun(context.Background(), b.ID, string(state.Auditing), 2); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
			{Severity: SeverityFix, Locus: "characters[0].name", Text: "x", Evidence: "y", Suggestion: "z"},
		}})
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	cfg := withSidecarAgent(t.TempDir(), fake)
	cfg.DB = db
	res, err := NewExecutor(cfg).Execute(context.Background(), b, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if res.AuditPassed {
		t.Error("AuditPassed = true, want false (unclean validation)")
	}
	if n := fake.count(string(state.Auditing)); n != 1 {
		t.Errorf("agent invoked %d times, want 1 (a real round)", n)
	}
	if _, err := os.Stat(auditAcceptedPath(b.WorkDir)); !os.IsNotExist(err) {
		t.Errorf("acceptance marker still present after a dirty re-entry (stat err %v), want removed", err)
	}
}

// TestAuditCleanPassWritesNoMarker: a genuine clean pass (fix==0) advances normally and
// leaves no trajectory artifacts.
func TestAuditCleanPassWritesNoMarker(t *testing.T) {
	db := openContribDB(t)
	b := auditBook(t, db, true)
	if _, err := db.StartStageRun(context.Background(), b.ID, string(state.Auditing), 1); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeOut(t, req, auditReportName, AuditReport{Pass: true, Findings: []AuditFinding{}})
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	cfg := withSidecarAgent(t.TempDir(), fake)
	cfg.DB = db
	res, err := NewExecutor(cfg).Execute(context.Background(), b, state.Auditing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if !res.AuditPassed {
		t.Error("AuditPassed = false, want true (clean pass)")
	}
	if _, err := os.Stat(auditAcceptedPath(b.WorkDir)); !os.IsNotExist(err) {
		t.Errorf("acceptance marker written on a clean pass (stat err %v), want none", err)
	}
	if _, err := os.Stat(auditRoundsPath(b.WorkDir)); !os.IsNotExist(err) {
		t.Errorf("round history written on a clean pass (stat err %v), want none", err)
	}
}

// --- scheduler-level end to end ---

// TestAuditAcceptsConvergingTrajectory drives the full sidecar loop: a converging audit
// (fix 3 -> fix 1) is ACCEPTED at round 2, the final fixing round applies its items, the
// auditing re-entry passes WITHOUT an agent invocation, and the book reaches done with
// the acceptance recorded on the marker and the contribution note.
func TestAuditAcceptsConvergingTrajectory(t *testing.T) {
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		switch req.Stage {
		case string(state.Synthesizing), string(state.Fixing):
			writeOutSidecars(t, req, "book")
		case string(state.Auditing):
			switch attempt {
			case 1:
				writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
					{Severity: SeverityFix, Locus: "a"}, {Severity: SeverityFix, Locus: "b"}, {Severity: SeverityFix, Locus: "c"},
					{Severity: SeverityNit, Locus: "n1"},
				}})
			case 2:
				writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: []AuditFinding{
					{Severity: SeverityFix, Locus: "d"},
					{Severity: SeverityNit, Locus: "n1"}, {Severity: SeverityNit, Locus: "n2"},
				}})
			default:
				t.Errorf("auditing agent invoked a 3rd time (attempt %d); the accepted re-entry must pass without the agent", attempt)
			}
		}
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	book, db, stop := startSidecarBook(t, fake)
	defer stop()

	final := waitState(t, db, book.ID, "done", 30*time.Second)
	if final.State != "done" {
		t.Fatalf("book state = %q (status %q err %q), want done", final.State, final.Status, final.Error)
	}
	// The agent ran audit rounds 1 and 2 only; the converged re-entry passed agentlessly.
	if n := fake.count(string(state.Auditing)); n != 2 {
		t.Errorf("audit agent invoked %d times, want 2 (round 3 must be agentless)", n)
	}
	// One accept round + the re-entry pass = 3 auditing successes; 2 fixing rounds.
	assertSuccesses(t, db, book.ID, string(state.Auditing), 3)
	assertSuccesses(t, db, book.ID, string(state.Fixing), 2)

	// The acceptance marker records the converging round + residual nits.
	acc, ok := loadAuditAccepted(book.WorkDir)
	if !ok {
		t.Fatal("audit_accepted.json missing")
	}
	if acc.Round != 2 || acc.Nit != 2 {
		t.Errorf("marker round=%d nit=%d, want 2/2", acc.Round, acc.Nit)
	}

	// The contribution rows carry the acceptance note (local mode).
	rows, err := db.ListContributionsByBook(context.Background(), book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no contribution rows")
	}
	for _, r := range rows {
		if !strings.Contains(r.Note, "converged after 2 rounds") || !strings.Contains(r.Note, "2 residual nit") {
			t.Errorf("row %s note = %q, want the acceptance line", r.Kind, r.Note)
		}
	}
}

// TestAuditGrowingFixParksWithTrajectory: a diverging audit (fix counts grow) never
// accepts and parks at the cap with the fix-count trajectory in the reason.
func TestAuditGrowingFixParksWithTrajectory(t *testing.T) {
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		switch req.Stage {
		case string(state.Synthesizing), string(state.Fixing):
			writeOutSidecars(t, req, "book")
		case string(state.Auditing):
			// A growing FIX count each round (1, 2, 3, ...) - never a non-growing
			// trajectory, so acceptTrajectory always refuses and the loop hits the cap.
			findings := make([]AuditFinding, attempt)
			for i := range findings {
				findings[i] = AuditFinding{Severity: SeverityFix, Locus: "f"}
			}
			writeOut(t, req, auditReportName, AuditReport{Pass: false, Findings: findings})
		}
		return agent.Result{Usage: agent.Usage{Model: "opus"}}, nil
	}
	book, db, stop := startSidecarBook(t, fake)
	defer stop()

	final := waitStatus(t, db, book.ID, string(state.StatusNeedsAttention), 30*time.Second)
	if final.State != string(state.Auditing) {
		t.Fatalf("parked at %q, want auditing", final.State)
	}
	if !strings.Contains(final.Error, "did not converge") || !strings.Contains(final.Error, "fix counts 1 -> 2 -> 3") {
		t.Errorf("park reason = %q, want the growing fix-count trajectory", final.Error)
	}
	assertSuccesses(t, db, book.ID, string(state.Fixing), state.MaxFixAttempts)
}
