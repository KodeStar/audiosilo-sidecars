package supervisor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

func kinds(in []Incident) map[IncidentKind]bool {
	m := map[IncidentKind]bool{}
	for _, i := range in {
		m[i.Kind] = true
	}
	return m
}

func TestClassifyStaleMissingProcessAndLimits(t *testing.T) {
	now := time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	r := store.StageRun{ID: 7, BookID: 1, Stage: "fact_pass", Attempt: 2, StartedAt: old, HeartbeatAt: old, ProgressAt: old,
		InputTokens: 500, OutputTokens: 400, CacheReadTokens: 200, CostUSD: 9, CostReported: true}
	s := Snapshot{Now: now, Book: store.Book{ID: 1, BatchID: "batch-a"}, Runs: []store.StageRun{r}, RuntimeActive: false}
	got := kinds(Classify(s, Policy{StaleAfter: time.Hour, NoProgressAfter: time.Hour, MaxStageDuration: time.Hour, MaxStageTokens: 1000, MaxStageCostUSD: 5, MaxErrorRepeats: 2}))
	for _, want := range []IncidentKind{IncidentMissingProcess, IncidentStaleHeartbeat, IncidentNoProgress, IncidentDurationLimit, IncidentTokenLimit, IncidentCostLimit} {
		if !got[want] {
			t.Errorf("missing %s in %#v", want, got)
		}
	}
}

func TestClassifyRecordedProcessDisappeared(t *testing.T) {
	now := time.Now().UTC()
	alive := false
	r := store.StageRun{ID: 1, Stage: "auditing", StartedAt: now.Format(time.RFC3339Nano), HeartbeatAt: now.Format(time.RFC3339Nano), ProgressAt: now.Format(time.RFC3339Nano)}
	got := Classify(Snapshot{Now: now, Book: store.Book{ID: 2, BatchID: "b"}, Runs: []store.StageRun{r}, RuntimeActive: true, ProcessAlive: &alive}, Policy{MaxErrorRepeats: 2})
	if len(got) != 1 || got[0].Kind != IncidentMissingProcess {
		t.Fatalf("incidents=%+v", got)
	}
}

func TestClassifyRunawayComparedWithPreviousSuccessfulAttempt(t *testing.T) {
	now := time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
	ok := true
	previous := store.StageRun{ID: 1, Stage: "fact_pass", Attempt: 1,
		StartedAt: now.Add(-40 * time.Minute).Format(time.RFC3339Nano), FinishedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		Ok: &ok, InputTokens: 100, OutputTokens: 50, CostUSD: 1, CostReported: true}
	current := store.StageRun{ID: 2, Stage: "fact_pass", Attempt: 2,
		StartedAt: now.Add(-31 * time.Minute).Format(time.RFC3339Nano), HeartbeatAt: now.Format(time.RFC3339Nano), ProgressAt: now.Format(time.RFC3339Nano),
		InputTokens: 400, OutputTokens: 100, CostUSD: 4, CostReported: true}
	got := kinds(Classify(Snapshot{Now: now, Book: store.Book{ID: 1, BatchID: "b"}, Runs: []store.StageRun{previous, current}, RuntimeActive: true}, Policy{MaxErrorRepeats: 2, AttemptGrowthFactor: 3}))
	for _, want := range []IncidentKind{IncidentDurationLimit, IncidentTokenLimit, IncidentCostLimit} {
		if !got[want] {
			t.Errorf("missing %s in %#v", want, got)
		}
	}
}

func TestClassifyCostLimitUsesEstimateWhenProviderCostIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	estimate := 6.0
	r := store.StageRun{ID: 3, Stage: "synthesizing", StartedAt: now.Format(time.RFC3339Nano),
		HeartbeatAt: now.Format(time.RFC3339Nano), ProgressAt: now.Format(time.RFC3339Nano),
		EstimatedAPICostUSD: &estimate, EstimateComplete: true}
	got := kinds(Classify(Snapshot{Now: now, Book: store.Book{ID: 3, BatchID: "b"}, Runs: []store.StageRun{r}, RuntimeActive: true}, Policy{MaxErrorRepeats: 2, MaxStageCostUSD: 5}))
	if !got[IncidentCostLimit] {
		t.Fatalf("incidents=%#v", got)
	}
}

func TestErrorFingerprintNormalizesVolatileDetails(t *testing.T) {
	a := ErrorFingerprint(`open /tmp/run-123/ch004.json: request abcdef123456 failed 42`)
	b := ErrorFingerprint(`open /var/work/run-999/ch088.json: request fedcba987654 failed 77`)
	if a != b {
		t.Fatalf("fingerprints differ: %s %s", a, b)
	}
}

func TestRepeatedErrorAndKnownBackendClasses(t *testing.T) {
	makeRun := func(id int64, msg string) store.StageRun {
		ok := false
		m, _ := json.Marshal(map[string]string{"error": msg})
		return store.StageRun{ID: id, Stage: "fact_pass", Attempt: int(id), FinishedAt: time.Now().Format(time.RFC3339Nano), Ok: &ok, Metrics: m}
	}
	base := Snapshot{Book: store.Book{ID: 1, BatchID: "b"}}
	base.Runs = []store.StageRun{makeRun(1, "open /tmp/a123 failed 99"), makeRun(2, "open /tmp/b456 failed 42")}
	got := Classify(base, Policy{MaxErrorRepeats: 2})
	if len(got) != 1 || got[0].Kind != IncidentRepeatedError {
		t.Fatalf("repeated=%+v", got)
	}
	for msg, want := range map[string]IncidentKind{"Not logged in · Please run /login": IncidentAuthentication, "HTTP 429 rate limit": IncidentRateLimit, "agent backend unavailable: cli not found on PATH": IncidentBackendUnavailable} {
		base.Runs = []store.StageRun{makeRun(1, msg)}
		got = Classify(base, Policy{MaxErrorRepeats: 2})
		if len(got) != 1 || got[0].Kind != want {
			t.Errorf("%q => %+v want %s", msg, got, want)
		}
	}
	base.Runs = []store.StageRun{makeRun(1, "schema did something novel")}
	got = Classify(base, Policy{MaxErrorRepeats: 2})
	if len(got) != 1 || got[0].Kind != IncidentUnclassified || !got[0].Ambiguous {
		t.Fatalf("unclassified=%+v", got)
	}
}

func TestQAAndAuditNonConvergence(t *testing.T) {
	ok := true
	qaMetrics := json.RawMessage(`{"multi_loop":4,"cross_segment":2,"mid_chapter_runs":1,"retranscribe_queue":1,"tail_rate":2,"within_segment":0,"wph_outliers":1}`)
	runs := []store.StageRun{{ID: 1, Stage: "qa_sweep", FinishedAt: "x", Ok: &ok, Metrics: qaMetrics}, {ID: 2, Stage: "qa_sweep", FinishedAt: "x", Ok: &ok, Metrics: qaMetrics}, {ID: 3, Stage: "qa_sweep", FinishedAt: "x", Ok: &ok, Metrics: qaMetrics}, {ID: 4, Stage: "auditing", FinishedAt: "x", Ok: &ok, Metrics: json.RawMessage(`{"pass":false,"fix":2}`)}, {ID: 5, Stage: "auditing", FinishedAt: "x", Ok: &ok, Metrics: json.RawMessage(`{"pass":false,"fix":3}`)}}
	got := kinds(Classify(Snapshot{Book: store.Book{ID: 1, BatchID: "b"}, Runs: runs}, Policy{MaxErrorRepeats: 2}))
	if !got[IncidentNonConvergingQA] || !got[IncidentNonConvergingAudit] {
		t.Fatalf("incidents=%#v", got)
	}
}

func TestDecisionRetryEscalationAndApprovalLimits(t *testing.T) {
	p := Policy{MaxAttempts: 3, AutomaticActions: true, AllowBackendFailover: false}
	d := Decide(Incident{Kind: IncidentMissingProcess}, 2, p)
	if d.Action != ActionTerminateRequeue || !d.Automatic {
		t.Fatalf("decision=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentMissingProcess}, 3, p)
	if d.Action != ActionParkEscalate || !d.ApprovalRequired || !d.Automatic {
		t.Fatalf("limit decision=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentArtifactInvalid, Stage: "contributing"}, 0, p)
	if d.Action != ActionParkEscalate || !d.ApprovalRequired {
		t.Fatalf("publishing decision=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentArtifactInvalid, Stage: "validating", Protected: true}, 0, p)
	if d.Action != ActionParkEscalate || d.Automatic || !d.ApprovalRequired {
		t.Fatalf("protected output decision=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentBackendUnavailable}, 0, p)
	if d.Action != ActionParkEscalate {
		t.Fatalf("backend=%+v", d)
	}
	p.AllowBackendFailover = true
	d = Decide(Incident{Kind: IncidentBackendUnavailable}, 0, p)
	if d.Action != ActionFallbackBackend || !d.Automatic {
		t.Fatalf("fallback=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentNoProgress}, 0, p)
	if d.Action != ActionParkEscalate || d.Automatic || !d.ApprovalRequired {
		t.Fatalf("ambiguous no-progress=%+v", d)
	}
	d = Decide(Incident{Kind: IncidentDurationLimit}, 0, p)
	if d.Action != ActionStopBudget || !d.Automatic {
		t.Fatalf("duration containment=%+v", d)
	}
}

func TestPrimaryIncidentPreventsConflictingAutomaticPlaybooks(t *testing.T) {
	got, ok := primaryIncident([]Incident{{Kind: IncidentNoProgress}, {Kind: IncidentCostLimit}, {Kind: IncidentMissingProcess}})
	if !ok || got.Kind != IncidentMissingProcess {
		t.Fatalf("primary=%+v ok=%v", got, ok)
	}
}
