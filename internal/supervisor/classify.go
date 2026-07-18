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
			var previous *store.StageRun
			for idx := range runs {
				candidate := &runs[idx]
				if candidate.ID != open.ID && candidate.Stage == open.Stage && candidate.FinishedAt != "" && candidate.Ok != nil && *candidate.Ok {
					previous = candidate
				}
			}
			if previous != nil {
				priorDuration := parseTime(previous.FinishedAt).Sub(parseTime(previous.StartedAt))
				if priorDuration >= time.Minute && elapsed > time.Duration(float64(priorDuration)*p.AttemptGrowthFactor) {
					i := base
					i.Kind = IncidentDurationLimit
					i.Diagnosis = "stage duration is excessive compared with its previous successful attempt"
					i.Evidence = []string{fmt.Sprintf("%s now versus %s previously (%.1fx limit)", elapsed.Round(time.Second), priorDuration.Round(time.Second), p.AttemptGrowthFactor)}
					incidents = append(incidents, i)
				}
				priorTokens := previous.InputTokens + previous.OutputTokens + previous.CacheReadTokens
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
	if s.Book.Status == string(state.StatusPaused) {
		for idx := range incidents {
			incidents[idx].Protected = true
		}
	}
	return dedupeIncidents(incidents)
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
	var qaFP []string
	var qaRunIDs []int64
	var auditFix []int
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
				auditFix = nil
				auditRunIDs = nil
			} else if f, ok := number(m["fix"]); ok {
				auditFix = append(auditFix, f)
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
	if auditPhase && len(auditFix) >= 2 && auditFix[len(auditFix)-1] >= auditFix[len(auditFix)-2] {
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
		k := incidentKey(i)
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
	d.Automatic = p.AutomaticActions && !i.Protected && d.Action != ActionObserve && d.Action != ActionAskModel &&
		(!d.ApprovalRequired || (d.Action == ActionParkEscalate && i.Kind != IncidentNoProgress && !i.Protected))
	return d
}
