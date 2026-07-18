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

type storeRun struct {
	id                    int64
	stage                 string
	attempt               int
	started, finished     string
	open                  bool
	input, output, cached int64
	cost                  float64
	costReported          bool
	estimatedCost         *float64
	estimateComplete      bool
	metrics               json.RawMessage
	ok                    *bool
	superseded            bool
	err                   string
}

func compactRuns(s Snapshot) []storeRun {
	out := make([]storeRun, 0, len(s.Runs))
	for _, r := range s.Runs {
		errMsg := ""
		var m map[string]any
		if json.Unmarshal(r.Metrics, &m) == nil {
			errMsg, _ = m["error"].(string)
		}
		out = append(out, storeRun{id: r.ID, stage: r.Stage, attempt: r.Attempt, started: r.StartedAt,
			finished: r.FinishedAt, open: r.FinishedAt == "", input: r.InputTokens, output: r.OutputTokens,
			cached: r.CacheReadTokens, cost: r.CostUSD, costReported: r.CostReported,
			estimatedCost: r.EstimatedAPICostUSD, estimateComplete: r.EstimateComplete,
			metrics: r.Metrics, ok: r.Ok, superseded: r.Superseded, err: errMsg})
	}
	return out
}

func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }

func Classify(s Snapshot, p Policy) []Incident {
	if s.Now.IsZero() {
		s.Now = time.Now().UTC()
	}
	runs := compactRuns(s)
	var incidents []Incident
	var open *storeRun
	for i := range runs {
		if runs[i].open {
			open = &runs[i]
			break
		}
	}
	if open != nil {
		base := Incident{BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: open.stage, StageRunID: open.id}
		if !s.RuntimeActive {
			i := base
			i.Kind = IncidentMissingProcess
			i.Diagnosis = "database stage is running but the scheduler has no worker"
			i.Evidence = []string{fmt.Sprintf("stage run %d is open", open.id)}
			incidents = append(incidents, i)
		} else if s.ProcessAlive != nil && !*s.ProcessAlive {
			i := base
			i.Kind = IncidentMissingProcess
			i.Diagnosis = "recorded invocation process has disappeared"
			i.Evidence = []string{"process_active is true but the pid is absent"}
			incidents = append(incidents, i)
		}
		actual := s.Runs[indexRun(s.Runs, open.id)]
		heartbeat := parseTime(actual.HeartbeatAt)
		if p.StaleAfter > 0 && !heartbeat.IsZero() && s.Now.Sub(heartbeat) > p.StaleAfter {
			i := base
			i.Kind = IncidentStaleHeartbeat
			i.Diagnosis = "stage heartbeat is stale"
			i.Evidence = []string{"last heartbeat " + actual.HeartbeatAt}
			incidents = append(incidents, i)
		}
		progress := parseTime(actual.ProgressAt)
		if p.NoProgressAfter > 0 && !progress.IsZero() && s.Now.Sub(progress) > p.NoProgressAfter {
			i := base
			i.Kind = IncidentNoProgress
			i.Diagnosis = "stage has made no meaningful progress"
			i.Evidence = []string{"last progress " + actual.ProgressAt}
			incidents = append(incidents, i)
		}
		started := parseTime(open.started)
		elapsed := s.Now.Sub(started)
		if p.MaxStageDuration > 0 && !started.IsZero() && elapsed > p.MaxStageDuration {
			i := base
			i.Kind = IncidentDurationLimit
			i.Diagnosis = "stage exceeded its duration limit"
			i.Evidence = []string{elapsed.Round(time.Second).String()}
			incidents = append(incidents, i)
		}
		tokens := open.input + open.output + open.cached
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
			var previous *storeRun
			for idx := range runs {
				candidate := &runs[idx]
				if candidate.id != open.id && candidate.stage == open.stage && candidate.finished != "" && candidate.ok != nil && *candidate.ok {
					previous = candidate
				}
			}
			if previous != nil {
				priorDuration := parseTime(previous.finished).Sub(parseTime(previous.started))
				if priorDuration >= time.Minute && elapsed > time.Duration(float64(priorDuration)*p.AttemptGrowthFactor) {
					i := base
					i.Kind = IncidentDurationLimit
					i.Diagnosis = "stage duration is excessive compared with its previous successful attempt"
					i.Evidence = []string{fmt.Sprintf("%s now versus %s previously (%.1fx limit)", elapsed.Round(time.Second), priorDuration.Round(time.Second), p.AttemptGrowthFactor)}
					incidents = append(incidents, i)
				}
				priorTokens := previous.input + previous.output + previous.cached
				if priorTokens > 0 && float64(tokens) > float64(priorTokens)*p.AttemptGrowthFactor {
					i := base
					i.Kind = IncidentTokenLimit
					i.Diagnosis = "stage token use is excessive compared with its previous successful attempt"
					i.Evidence = []string{fmt.Sprintf("%d now versus %d previously (%.1fx limit)", tokens, priorTokens, p.AttemptGrowthFactor)}
					incidents = append(incidents, i)
				}
				previousCost, previousCostKind, previousCostKnown := comparableCost(*previous)
				if previousCostKnown && openCostKnown && previousCostKind == openCostKind && previousCost > 0 && openCost > previousCost*p.AttemptGrowthFactor {
					i := base
					i.Kind = IncidentCostLimit
					i.Diagnosis = "stage cost is excessive compared with its previous successful attempt"
					i.Evidence = []string{fmt.Sprintf("$%.4f now versus $%.4f previously, %s (%.1fx limit)", openCost, previousCost, openCostKind, p.AttemptGrowthFactor)}
					incidents = append(incidents, i)
				}
			}
		}
	}

	incidents = append(incidents, classifyFailures(s, runs, p)...)
	incidents = append(incidents, classifyConvergence(s, runs)...)
	for _, a := range s.Artifacts {
		if a.Valid {
			continue
		}
		protected := s.Book.State == "ready" || s.Book.State == "contributing" || s.Book.State == "done"
		incidents = append(incidents, Incident{Kind: IncidentArtifactInvalid, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: a.Stage, StageRunID: a.StageRunID,
			Diagnosis: "required artifact or completion sentinel is missing or invalid", Evidence: []string{a.Path, a.Reason}, Protected: protected})
	}
	if s.AgentCapacity > 0 && s.AgentActive < s.AgentCapacity && s.EligibleAgentBooks > 0 {
		occupancy := fmt.Sprintf("%d/%d active; %d eligible", s.AgentActive, s.AgentCapacity, s.EligibleAgentBooks)
		incidents = append(incidents, Incident{Kind: IncidentSlotInefficiency, BookID: s.Book.ID, BatchID: s.Book.BatchID,
			Diagnosis: "agent capacity is idle while eligible books are queued", Fingerprint: ErrorFingerprint(occupancy), Evidence: []string{occupancy}})
	}
	return dedupeIncidents(incidents)
}

func comparableCost(r storeRun) (float64, string, bool) {
	if r.costReported {
		return r.cost, "provider-reported", true
	}
	if r.estimateComplete && r.estimatedCost != nil {
		return *r.estimatedCost, "API-equivalent estimate", true
	}
	return 0, "unavailable", false
}

// private error field (declared here to avoid exposing it in public snapshots).
func indexRun(runs []store.StageRun, id int64) int {
	for i := range runs {
		if runs[i].ID == id {
			return i
		}
	}
	return 0
}

func classifyFailures(s Snapshot, runs []storeRun, p Policy) []Incident {
	var failures []storeRun
	for _, r := range runs {
		if r.err != "" {
			failures = append(failures, r)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	last := failures[len(failures)-1]
	base := Incident{BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: last.stage, StageRunID: last.id, Fingerprint: ErrorFingerprint(last.err), Evidence: []string{truncate(last.err, 240)}}
	if kind := classifyError(last.err); kind != "" {
		base.Kind = kind
		base.Diagnosis = string(kind)
		return []Incident{base}
	}
	repeats := 0
	for i := len(failures) - 1; i >= 0; i-- {
		if failures[i].stage == last.stage && ErrorFingerprint(failures[i].err) == base.Fingerprint {
			repeats++
		}
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

func classifyConvergence(s Snapshot, runs []storeRun) []Incident {
	var qaFP []string
	var qaRunIDs []int64
	var auditFix []int
	var auditRunIDs []int64
	for _, r := range runs {
		if r.superseded {
			continue
		}
		var m map[string]any
		if json.Unmarshal(r.metrics, &m) != nil {
			continue
		}
		if r.stage == "qa_sweep" {
			qaFP = append(qaFP, metricFingerprint(m, []string{"cross_segment", "mid_chapter_runs", "multi_loop", "retranscribe_queue", "tail_rate", "within_segment", "wph_outliers"}))
			qaRunIDs = append(qaRunIDs, r.id)
		}
		if r.stage == "auditing" {
			if pass, _ := m["pass"].(bool); !pass {
				if f, ok := number(m["fix"]); ok {
					auditFix = append(auditFix, f)
					auditRunIDs = append(auditRunIDs, r.id)
				}
			}
		}
	}
	var out []Incident
	if len(qaFP) >= 3 && qaFP[len(qaFP)-1] == qaFP[len(qaFP)-2] && qaFP[len(qaFP)-2] == qaFP[len(qaFP)-3] {
		out = append(out, Incident{Kind: IncidentNonConvergingQA, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: "qa_sweep", StageRunID: qaRunIDs[len(qaRunIDs)-1], Fingerprint: qaFP[len(qaFP)-1], Diagnosis: "QA repair loop repeated the same findings", Evidence: []string{"three identical QA fingerprints"}})
	}
	if len(auditFix) >= 2 && auditFix[len(auditFix)-1] >= auditFix[len(auditFix)-2] {
		out = append(out, Incident{Kind: IncidentNonConvergingAudit, BookID: s.Book.ID, BatchID: s.Book.BatchID, Stage: "auditing", StageRunID: auditRunIDs[len(auditRunIDs)-1], Diagnosis: "audit repair loop is flat or diverging", Evidence: []string{fmt.Sprintf("fix counts %d -> %d", auditFix[len(auditFix)-2], auditFix[len(auditFix)-1])}})
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
		k := fmt.Sprintf("%s/%d/%s/%d/%s", i.Kind, i.BookID, i.Stage, i.StageRunID, i.Fingerprint)
		if !seen[k] {
			seen[k] = true
			out = append(out, i)
		}
	}
	return out
}

func Decide(i Incident, attempts int, p Policy) Decision {
	d := Decision{Incident: i, Action: ActionObserve, RetryLimit: p.MaxAttempts, TerminationLimit: 1}
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
	case IncidentAuthentication, IncidentRepeatedError, IncidentNonConvergingQA, IncidentNonConvergingAudit:
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
	// ApprovalRequired can coexist with an automatic park: containment is automatic,
	// while re-admission/remediation remains a human decision.
	d.Automatic = p.AutomaticActions && d.Action != ActionObserve && d.Action != ActionAskModel &&
		(!d.ApprovalRequired || (d.Action == ActionParkEscalate && i.Kind != IncidentNoProgress && !i.Protected))
	return d
}
