package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

var (
	pathPattern  = regexp.MustCompile(`(?:[A-Za-z]:)?[/\\][^\s"']+`)
	idPattern    = regexp.MustCompile(`\b(?:[0-9a-f]{8,}|\d+)\b`)
	spacePattern = regexp.MustCompile(`\s+`)
)

// ErrorFingerprint normalizes volatile paths, ids and counters before hashing so
// retries of the same underlying failure compare equal.
func ErrorFingerprint(message string) string {
	n := strings.ToLower(strings.TrimSpace(message))
	n = pathPattern.ReplaceAllString(n, "<path>")
	n = idPattern.ReplaceAllString(n, "<n>")
	n = spacePattern.ReplaceAllString(n, " ")
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:12])
}

func classifyError(message string) IncidentKind {
	low := strings.ToLower(message)
	for _, sig := range []string{"not logged in", "authentication", "unauthorized", "invalid api key", "401"} {
		if strings.Contains(low, sig) {
			return IncidentAuthentication
		}
	}
	for _, sig := range []string{"rate limit", "rate_limit", "too many requests", "429", "overloaded", "usage limit"} {
		if strings.Contains(low, sig) {
			return IncidentRateLimit
		}
	}
	for _, sig := range []string{"backend unavailable", "cli not found", "not found on path", "connection refused", "service unavailable", "503"} {
		if strings.Contains(low, sig) {
			return IncidentBackendUnavailable
		}
	}
	return ""
}

func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }

// minGrowthElapsed is the absolute floor below which the relative attempt-growth
// duration check stays silent. Stage-relative growth is an early warning meant to
// catch a runaway well before MaxStageDuration; firing it on a run measured in
// minutes only reports ordinary agent-workload variance.
const minGrowthElapsed = 20 * time.Minute

// attemptBaseline is the high-water mark of a stage's successful attempts. The
// question the growth checks ask is "has this attempt gone beyond anything the
// stage has ever legitimately needed", so the baseline is the maximum rather than
// whichever attempt happened to finish last - loop stages (auditing, fixing,
// qa_adjudicating) run repeatedly over workloads of very different sizes, which
// makes the most recent attempt an arbitrary sample.
type attemptBaseline struct {
	duration time.Duration
	tokens   int64
	cost     float64
}

// priorSuccessBaseline folds every successful earlier attempt of open's stage into
// that high-water mark. Cost only accumulates from attempts whose cost is known and
// of the same kind as open's (reported provider cost and local estimate are not
// comparable), so a mixed history never compares the two against each other.
func priorSuccessBaseline(runs []store.StageRun, open store.StageRun, openCostKind string, openCostKnown bool) attemptBaseline {
	var b attemptBaseline
	for idx := range runs {
		r := runs[idx]
		if r.ID == open.ID || r.Stage != open.Stage || r.FinishedAt == "" || r.Ok == nil || !*r.Ok {
			continue
		}
		if d := parseTime(r.FinishedAt).Sub(parseTime(r.StartedAt)); d > b.duration {
			b.duration = d
		}
		if t := r.InputTokens + r.OutputTokens + r.CacheReadTokens; t > b.tokens {
			b.tokens = t
		}
		if cost, kind, known := comparableCost(r); known && openCostKnown && kind == openCostKind && cost > b.cost {
			b.cost = cost
		}
	}
	return b
}

func stageRunError(r store.StageRun) string {
	var metrics map[string]any
	if json.Unmarshal(r.Metrics, &metrics) != nil {
		return ""
	}
	errMessage, _ := metrics["error"].(string)
	return errMessage
}

func Classify(s Snapshot, p Policy) []Incident {
	if s.Now.IsZero() {
		s.Now = time.Now().UTC()
	}
	runs := s.Runs
	var incidents []Incident
	var open *store.StageRun
	for i := range runs {
		if runs[i].FinishedAt == "" {
			open = &runs[i]
			break
		}
	}
	if open != nil {
		base := Incident{BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: open.Stage, StageRunID: open.ID}
		if !s.RuntimeActive {
			i := base
			i.Kind = IncidentMissingProcess
			i.Diagnosis = "database stage is running but the scheduler has no worker"
			i.Evidence = []string{fmt.Sprintf("stage run %d is open", open.ID)}
			incidents = append(incidents, i)
		} else if s.ProcessAlive != nil && !*s.ProcessAlive {
			i := base
			i.Kind = IncidentMissingProcess
			i.Diagnosis = "recorded invocation process has disappeared"
			i.Evidence = []string{"process_active is true but the pid is absent"}
			incidents = append(incidents, i)
		}
		heartbeat := parseTime(open.HeartbeatAt)
		if p.StaleAfter > 0 && !heartbeat.IsZero() && s.Now.Sub(heartbeat) > p.StaleAfter {
			i := base
			i.Kind = IncidentStaleHeartbeat
			i.Diagnosis = "stage heartbeat is stale"
			i.Evidence = []string{"last heartbeat " + open.HeartbeatAt}
			incidents = append(incidents, i)
		}
		progress := parseTime(open.ProgressAt)
		if p.NoProgressAfter > 0 && !progress.IsZero() && s.Now.Sub(progress) > p.NoProgressAfter {
			i := base
			i.Kind = IncidentNoProgress
			i.Diagnosis = "stage has made no meaningful progress"
			i.Evidence = []string{"last progress " + open.ProgressAt}
			incidents = append(incidents, i)
		}
		started := parseTime(open.StartedAt)
		elapsed := s.Now.Sub(started)
		if p.MaxStageDuration > 0 && !started.IsZero() && elapsed > p.MaxStageDuration {
			i := base
			i.Kind = IncidentDurationLimit
			i.Diagnosis = "stage exceeded its duration limit"
			i.Evidence = []string{elapsed.Round(time.Second).String()}
			incidents = append(incidents, i)
		}
		tokens := open.InputTokens + open.OutputTokens + open.CacheReadTokens
		if p.MaxStageTokens > 0 && tokens >= p.MaxStageTokens {
			i := base
			i.Kind = IncidentTokenLimit
			i.Diagnosis = "stage reached its token limit"
			i.Evidence = []string{strconv.FormatInt(tokens, 10) + " tokens"}
			incidents = append(incidents, i)
		}
		openCost, openCostKind, openCostKnown := comparableCost(*open)
		if p.MaxStageCostUSD > 0 && openCostKnown && openCost >= p.MaxStageCostUSD {
			i := base
			i.Kind = IncidentCostLimit
			i.Diagnosis = "stage reached its configured cost limit"
			i.Evidence = []string{fmt.Sprintf("$%.4f %s", openCost, openCostKind)}
			incidents = append(incidents, i)
		}
		if p.AttemptGrowthFactor > 1 {
			prior := priorSuccessBaseline(runs, *open, openCostKind, openCostKnown)
			// The relative checks only fire once the open attempt is itself large enough
			// to be suspicious on its own. An agent stage's cost is a function of the work
			// it was handed, not of drift: a fixing round carrying three blockers
			// legitimately runs several times longer than one carrying a small edit, so a
			// short earlier attempt is a sample, not a budget. Without minGrowthElapsed
			// this killed a healthy 3m13s fixing run whose predecessor happened to take
			// 62s - 1.8% of the absolute MaxStageDuration the check is meant to pre-empt.
			if prior.duration >= time.Minute && elapsed >= minGrowthElapsed && elapsed > time.Duration(float64(prior.duration)*p.AttemptGrowthFactor) {
				i := base
				i.Kind = IncidentDurationLimit
				i.Diagnosis = "stage duration is excessive compared with its longest successful attempt"
				i.Evidence = []string{fmt.Sprintf("%s now versus %s at its longest (%.1fx limit)", elapsed.Round(time.Second), prior.duration.Round(time.Second), p.AttemptGrowthFactor)}
				incidents = append(incidents, i)
			}
			if prior.tokens > 0 && float64(tokens) > float64(prior.tokens)*p.AttemptGrowthFactor {
				i := base
				i.Kind = IncidentTokenLimit
				i.Diagnosis = "stage token use is excessive compared with its heaviest successful attempt"
				i.Evidence = []string{fmt.Sprintf("%d now versus %d at its heaviest (%.1fx limit)", tokens, prior.tokens, p.AttemptGrowthFactor)}
				incidents = append(incidents, i)
			}
			if openCostKnown && prior.cost > 0 && openCost > prior.cost*p.AttemptGrowthFactor {
				i := base
				i.Kind = IncidentCostLimit
				i.Diagnosis = "stage cost is excessive compared with its most expensive successful attempt"
				i.Evidence = []string{fmt.Sprintf("$%.4f now versus $%.4f at its most expensive, %s (%.1fx limit)", openCost, prior.cost, openCostKind, p.AttemptGrowthFactor)}
				incidents = append(incidents, i)
			}
		}
	}

	incidents = append(incidents, classifyFailures(s, runs, p)...)
	incidents = append(incidents, classifyConvergence(s, runs)...)
	if parked, ok := classifyParked(s.Book, runs); ok {
		incidents = append(incidents, parked)
	}
	for _, a := range s.Artifacts {
		if a.Valid {
			continue
		}
		protected := s.Book.State == "ready" || s.Book.State == "contributing" || s.Book.State == "done"
		incidents = append(incidents, Incident{Kind: IncidentArtifactInvalid, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: a.Stage, StageRunID: a.StageRunID,
			Diagnosis: "required artifact or completion sentinel is missing or invalid", Evidence: []string{a.Path, a.Reason}, Protected: protected})
	}
	bookSlotIdle := s.AgentCapacity > 0 && s.AgentActive < s.AgentCapacity && s.EligibleAgentBooks > 0
	invocationSlotIdle := state.SupportsAgentFanout(state.State(s.Book.State)) && s.RuntimeActive && s.BookInvocations > 0 &&
		s.BookInvocations < s.MaxAgentsPerBook && s.AgentInvocations < s.InvocationCapacity && s.RemainingWorkUnits > s.BookInvocations
	if bookSlotIdle || invocationSlotIdle {
		occupancy := fmt.Sprintf("books %d/%d active; %d eligible; invocations %d/%d global, %d/%d for book; %d work units remaining",
			s.AgentActive, s.AgentCapacity, s.EligibleAgentBooks, s.AgentInvocations, s.InvocationCapacity,
			s.BookInvocations, s.MaxAgentsPerBook, s.RemainingWorkUnits)
		incidents = append(incidents, Incident{Kind: IncidentSlotInefficiency, BookID: s.Book.ID, BatchID: s.Book.BatchID,
			Diagnosis: "agent capacity is idle while eligible book or invocation work is queued", Fingerprint: ErrorFingerprint(occupancy), Evidence: []string{occupancy}})
	}
	if s.Book.Status == string(state.StatusPaused) {
		for idx := range incidents {
			incidents[idx].Protected = true
		}
	}
	return dedupeIncidents(incidents)
}

// classifyParked turns durable needs-attention state into a first-class recovery
// incident. Previously the supervisor handled only the failure that caused a park;
// once containment was recorded it never planned readmission. The latest stage-run
// id identifies each new recovery attempt, while the normalized fingerprint lets
// the service cap repeated recovery of the same underlying problem.
func classifyParked(book store.Book, runs []store.StageRun) (Incident, bool) {
	if book.Status != string(state.StatusNeedsAttention) || book.ParkCode == "" {
		return Incident{}, false
	}
	// One reverse walk over the current stage's runs: take the latest NON-SUPERSEDED run so the
	// incident's StageRunID and its fingerprint derive from the SAME run. Previously the id walk
	// ignored Superseded while the fingerprint's error walk filtered it, so a superseded retry
	// could identify the incident while an older run supplied the error text.
	var stageRunID int64
	var stageErr string
	for idx := len(runs) - 1; idx >= 0; idx-- {
		if runs[idx].Stage == book.State && !runs[idx].Superseded {
			stageRunID = runs[idx].ID
			stageErr = stageRunError(runs[idx])
			break
		}
	}
	evidence := []string{"park code " + book.ParkCode}
	if message := strings.TrimSpace(book.Error); message != "" {
		evidence = append(evidence, truncate(message, 240))
	}
	return Incident{
		Kind: IncidentParkedRecovery, BookID: book.ID, BatchID: book.BatchID,
		Stage: book.State, StageRunID: stageRunID, ParkCode: book.ParkCode,
		Fingerprint: parkedRecoveryFingerprint(book, stageErr),
		Diagnosis:   "parked book requires a bounded recovery plan", Evidence: evidence,
	}, true
}

// parkedRecoveryFingerprint derives a STATIONARY fingerprint for a parked-recovery
// incident. book.Error must never contribute: every supervisor park_escalate rewrites
// it to "supervisor: <diagnosis>: <evidence>", nesting a re-truncated copy of the
// previous error, so hashing it produced a fresh fingerprint each recovery cycle and
// the family-attempt cap never bound (the incident ping-ponged indefinitely). Instead
// hash stable material only: the park code + current stage, plus the underlying failed
// stage-run error for that stage when one exists (the genuine ffmpeg/agent text, which the
// supervisor never rewrites). classifyParked supplies that error from the SAME non-superseded
// run it takes the StageRunID from. A supervisor message prefix on the error is stripped so no
// escalation prose can leak back in.
func parkedRecoveryFingerprint(book store.Book, underlyingErr string) string {
	source := book.ParkCode + " " + book.State
	if underlying := stripSupervisorPrefix(underlyingErr); underlying != "" {
		source += " " + underlying
	}
	return ErrorFingerprint(source)
}

// stripSupervisorPrefix removes a leading supervisor message prefix (state.SupervisorMessagePrefix,
// e.g. "supervisor: <text>") so a re-hashed escalation message never contaminates the stationary
// parked-recovery fingerprint. It trims the colon form (prefix without its trailing space) so it
// tolerates a prefix written with or without the space.
func stripSupervisorPrefix(message string) string {
	trimmed := strings.TrimSpace(message)
	trimmed = strings.TrimPrefix(trimmed, strings.TrimSpace(state.SupervisorMessagePrefix))
	return strings.TrimSpace(trimmed)
}

func comparableCost(r store.StageRun) (float64, string, bool) {
	if r.CostReported {
		return r.CostUSD, "provider-reported", true
	}
	if r.EstimateComplete && r.EstimatedAPICostUSD != nil {
		return *r.EstimatedAPICostUSD, "API-equivalent estimate", true
	}
	return 0, "unavailable", false
}

func classifyFailures(s Snapshot, runs []store.StageRun, p Policy) []Incident {
	var stageRuns []store.StageRun
	for _, r := range runs {
		if r.Stage == s.Book.State && !r.Superseded {
			stageRuns = append(stageRuns, r)
		}
	}
	if len(stageRuns) == 0 {
		return nil
	}
	last := stageRuns[len(stageRuns)-1]
	lastError := stageRunError(last)
	if lastError == "" || last.Ok == nil || *last.Ok {
		return nil
	}
	base := Incident{BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: last.Stage, StageRunID: last.ID, Fingerprint: ErrorFingerprint(lastError), Evidence: []string{truncate(lastError, 240)}}
	if kind := classifyError(lastError); kind != "" {
		base.Kind = kind
		base.Diagnosis = string(kind)
		return []Incident{base}
	}
	repeats := 0
	for idx := len(stageRuns) - 1; idx >= 0; idx-- {
		errMessage := stageRunError(stageRuns[idx])
		if errMessage == "" || ErrorFingerprint(errMessage) != base.Fingerprint {
			break
		}
		repeats++
	}
	if repeats >= p.MaxErrorRepeats {
		base.Kind = IncidentRepeatedError
		base.Diagnosis = "retries are producing the same error fingerprint"
		base.Evidence = append(base.Evidence, fmt.Sprintf("%d matching attempts", repeats))
		return []Incident{base}
	}
	base.Kind = IncidentUnclassified
	base.Diagnosis = "stage failure does not match a deterministic incident class"
	base.Ambiguous = true
	return []Incident{base}
}

func classifyConvergence(s Snapshot, runs []store.StageRun) []Incident {
	type auditCounts struct{ blocker, fix int }
	var qaFP []string
	var qaRunIDs []int64
	var auditHistory []auditCounts
	var auditRunIDs []int64
	for _, r := range runs {
		if r.Superseded || r.Ok == nil || !*r.Ok {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.Metrics, &m) != nil {
			continue
		}
		if r.Stage == "qa_sweep" {
			qaFP = append(qaFP, metricFingerprint(m, []string{"cross_segment", "mid_chapter_runs", "multi_loop", "retranscribe_queue", "tail_rate", "within_segment", "wph_outliers"}))
			qaRunIDs = append(qaRunIDs, r.ID)
		}
		if r.Stage == "auditing" {
			if pass, _ := m["pass"].(bool); pass {
				auditHistory = nil
				auditRunIDs = nil
			} else if f, ok := number(m["fix"]); ok {
				blocker, _ := number(m["blocker"])
				auditHistory = append(auditHistory, auditCounts{blocker: blocker, fix: f})
				auditRunIDs = append(auditRunIDs, r.ID)
			}
		}
	}
	var out []Incident
	qaPhase := s.Book.State == "qa_sweep" || s.Book.State == "qa_adjudicating" || s.Book.State == "retranscribing"
	if qaPhase && len(qaFP) >= 3 && qaFP[len(qaFP)-1] == qaFP[len(qaFP)-2] && qaFP[len(qaFP)-2] == qaFP[len(qaFP)-3] {
		out = append(out, Incident{Kind: IncidentNonConvergingQA, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: "qa_sweep", StageRunID: qaRunIDs[len(qaRunIDs)-1], Fingerprint: qaFP[len(qaFP)-1], Diagnosis: "QA repair loop repeated the same findings", Evidence: []string{"three identical QA fingerprints"}})
	}
	auditPhase := s.Book.State == "auditing" || s.Book.State == "fixing"
	if auditPhase && len(auditHistory) >= 2 {
		previous, current := auditHistory[len(auditHistory)-2], auditHistory[len(auditHistory)-1]
		previousTotal, currentTotal := previous.blocker+previous.fix, current.blocker+current.fix
		// A blocker becoming a fix is a severity improvement even when the total
		// stays flat. Only diagnose a non-improving severity/count trajectory.
		if current.blocker >= previous.blocker && currentTotal >= previousTotal {
			evidence := fmt.Sprintf("actionable findings %d (%d blocker, %d fix) -> %d (%d blocker, %d fix)",
				previousTotal, previous.blocker, previous.fix, currentTotal, current.blocker, current.fix)
			out = append(out, Incident{Kind: IncidentNonConvergingAudit, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: "auditing", StageRunID: auditRunIDs[len(auditRunIDs)-1], Diagnosis: "audit findings are flat or diverging; the bounded pipeline repair loop remains in control", Evidence: []string{evidence}})
		}
	}
	return out
}

func metricFingerprint(m map[string]any, keys []string) string {
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v;", k, m[k])
	}
	return ErrorFingerprint(b.String())
}
func number(v any) (int, bool) { f, ok := v.(float64); return int(f), ok }
func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n] + "…"
}
func dedupeIncidents(in []Incident) []Incident {
	seen := map[string]bool{}
	out := make([]Incident, 0, len(in))
	for _, i := range in {
		k := incidentKey(i)
		if !seen[k] {
			seen[k] = true
			out = append(out, i)
		}
	}
	return out
}

// autoRecoveryActions is the set of AUTOMATIC recovery actions the durable cross-Kind cap
// counts and gates: each re-admits or re-runs paid work, so an unbounded cycle of any of them
// (not just retry/readmit) burns budget on a book that will not converge. terminate_requeue is
// deliberately EXCLUDED: it is orphaned-process hygiene (a daemon restart legitimately fires
// several in a day to clean up dead workers), so counting it would park healthy books after
// every restart. The store's CountAutoRecoveriesSince SQL IN clause mirrors this exact set.
var autoRecoveryActions = map[Action]bool{
	ActionRetry: true, ActionReadmit: true, ActionSupersedeRerun: true,
	ActionRequeue: true, ActionFallbackBackend: true,
}

// isAutoRecoveryAction reports whether an action is one the durable auto-recovery cap counts
// and gates. It is the ONE definition shared by capsAutoRecovery and the service's pre-gate.
func isAutoRecoveryAction(a Action) bool { return autoRecoveryActions[a] }

// capsAutoRecovery reports whether the durable cross-Kind auto-recovery cap binds for a
// tentative decision d: a would-be automatic auto-recovery action (isAutoRecoveryAction) for a
// book that has already had at least MaxAttempts such actions in the caller's rolling window
// (recentAutoRecoveries, supplied by the service). The per-family attempt count can be
// defeated by Kind alternation or fingerprint drift, so this blunt count is the backstop.
// When it binds, Decide converts the action to park_escalate + approval-required and lets
// its normal Automatic formula recompute (park_escalate stays automatic containment;
// re-admission stops). It is checked before Automatic is computed, so it gates on the
// would-be-automatic condition (p.AutomaticActions && !i.Protected) directly.
func capsAutoRecovery(d Decision, recentAutoRecoveries int, i Incident, p Policy) bool {
	return p.MaxAttempts > 0 && recentAutoRecoveries >= p.MaxAttempts &&
		isAutoRecoveryAction(d.Action) &&
		p.AutomaticActions && !i.Protected
}

func Decide(i Incident, attempts, recentAutoRecoveries int, p Policy) Decision {
	d := Decision{Incident: i, Action: ActionObserve, RetryLimit: p.MaxAttempts, TerminationLimit: 1}
	if i.Kind == IncidentParkedRecovery {
		switch state.ParkCode(i.ParkCode) {
		case state.ParkAgentUnavailable, state.ParkAgentRateLimited:
			d.Action = ActionReadmit
		case state.ParkQANoConverge, state.ParkFixLoopExhausted,
			state.ParkAgentValidationExhausted, state.ParkSpellingGateFailure,
			state.ParkSupervisorBudget:
			// These paths preserve and validate their durable inputs. A bounded fresh
			// pass is more useful than waiting for a human to click the same Retry.
			d.Action = ActionRetry
		case state.ParkSupervisorEscalated:
			if p.ModelAssisted {
				d.Action = ActionAskModel
			} else {
				d.Action = ActionRetry
			}
		case state.ParkMarkersNotConfident, state.ParkManifestChanged:
			if p.ModelAssisted {
				d.Action = ActionAskModel
			} else {
				d.Action = ActionParkEscalate
				d.ApprovalRequired = true
			}
		default:
			// Book budget, contribution/core, and missing-tool parks express an
			// external precondition. Restart performs a one-shot availability probe;
			// periodic retries here would churn forever.
			d.Action = ActionObserve
			d.ApprovalRequired = true
		}
		if attempts >= p.MaxAttempts && d.Action != ActionObserve {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		}
		if capsAutoRecovery(d, recentAutoRecoveries, i, p) {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		}
		d.Automatic = p.AutomaticActions && !i.Protected && d.Action != ActionObserve && d.Action != ActionAskModel &&
			(!d.ApprovalRequired || d.Action == ActionParkEscalate)
		return d
	}
	switch i.Kind {
	case IncidentMissingProcess, IncidentStaleHeartbeat:
		d.Action = ActionTerminateRequeue
	case IncidentDurationLimit, IncidentTokenLimit, IncidentCostLimit:
		d.Action = ActionStopBudget
	case IncidentNoProgress:
		if p.ModelAssisted {
			d.Action = ActionAskModel
		} else {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		}
	case IncidentRateLimit:
		d.Action = ActionReadmit
	case IncidentBackendUnavailable:
		if p.AllowBackendFailover {
			d.Action = ActionFallbackBackend
		} else {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		}
	case IncidentArtifactInvalid:
		if i.Stage == "contributing" || i.Protected {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		} else {
			d.Action = ActionSupersedeRerun
		}
	case IncidentSlotInefficiency:
		d.Action = ActionReallocate
	case IncidentNonConvergingQA, IncidentNonConvergingAudit:
		// QA and audit/fix loops already have bounded, artifact-aware recovery in
		// the production pipeline. The supervisor may diagnose their trajectory,
		// but must not interrupt a corrective pass or replace the pipeline's rich
		// typed park reason with a generic supervisor escalation.
		d.Action = ActionObserve
	case IncidentAuthentication, IncidentRepeatedError:
		d.Action = ActionParkEscalate
		d.ApprovalRequired = true
	default:
		if p.ModelAssisted {
			d.Action = ActionAskModel
		} else {
			d.Action = ActionParkEscalate
			d.ApprovalRequired = true
		}
	}
	if attempts >= p.MaxAttempts && (d.Action == ActionRetry || d.Action == ActionReadmit || d.Action == ActionRequeue || d.Action == ActionTerminateRequeue || d.Action == ActionSupersedeRerun) {
		d.Action = ActionParkEscalate
		d.ApprovalRequired = true
	}
	if capsAutoRecovery(d, recentAutoRecoveries, i, p) {
		d.Action = ActionParkEscalate
		d.ApprovalRequired = true
	}
	// ApprovalRequired can coexist with an automatic park: containment is automatic,
	// while re-admission/remediation remains a human decision.
	d.Automatic = p.AutomaticActions && !i.Protected && d.Action != ActionObserve && d.Action != ActionAskModel &&
		(!d.ApprovalRequired || (d.Action == ActionParkEscalate && i.Kind != IncidentNoProgress && !i.Protected))
	return d
}
