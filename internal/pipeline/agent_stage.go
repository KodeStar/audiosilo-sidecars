package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/agent/prompts"
	"github.com/kodestar/audiosilo-sidecars/internal/asr"
	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/qa"
	"github.com/kodestar/audiosilo-sidecars/internal/repair"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/spelling"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// AgentUnavailableMsg is the needs_attention reason an agent stage parks a book with
// when no agent backend can run (no claude/codex CLI resolved, or an explicit
// configured path is missing). It is a human-fixable precondition - install a CLI or
// fix the config path, then Retry - so parking (which Retry re-admits) fits better
// than a hard failure, exactly like MediaToolsUnavailableMsg for the audio stages.
const AgentUnavailableMsg = "agent backend unavailable - install the claude or codex CLI, or set agent.backend/claude_path/codex_path in config.yaml, then Retry"

// Park-reason strings/prefixes for the remaining agent/spelling park conditions. They
// are consts (not inline at the throw site) so the tests that assert them cite one
// source of truth. Typed ParkReason codes are deferred to M6; these are just strings.
const (
	// AgentRateLimitedPrefix prefixes the park reason when the agent backend is
	// rate-limited (the backend's detail + a "retry later" tail follow).
	AgentRateLimitedPrefix = "agent backend is rate-limited"
	// AgentValidationExhaustedPrefix prefixes the park reason when an agent's output
	// fails validation after the retry budget (the validator error follows).
	AgentValidationExhaustedPrefix = "agent output failed validation after retries"
	// MarkersNotConfidentPrefix prefixes the park reason when marker normalization
	// needs a human (the agent's verdict reason follows).
	MarkersNotConfidentPrefix = "marker normalization needs a human"
	// QAStalledPrefix prefixes the park reason when a QA repair round made no progress
	// (the retranscribing stage neither spliced nor adopted any chapter), so the loop is
	// stuck: the next qa_sweep re-flags the same chapters and another agent round would
	// cost money to reach the same no-op. The stuck chapters follow. This progress-based
	// signal (see retranscribeStalledMarker) replaced the old report+ledger fingerprint,
	// which mutated on every futile re-attempt (each CLIP-REDEGENERATED verdict relocated
	// its clip_start) so the fixed point never fired and the book burned its round budget.
	QAStalledPrefix = "QA adjudication stalled - repairs stopped making progress"
	// SpellingGateFailurePrefix prefixes the park reason when the spelling gate Check
	// fails (the gate summary follows).
	SpellingGateFailurePrefix = "spelling corrections failed the gates - fix spelling_research and retry"
)

// budgetExceededMsg is the park reason when a book's summed agent cost has reached the
// configured per-book budget. It names the spend and the budget and the exact fix, so a
// user knows the one lever (raise agent.book_budget_usd, restart, Retry).
func budgetExceededMsg(spent, budget float64) string {
	return fmt.Sprintf("book agent cost $%.2f reached the budget $%.2f - raise agent.book_budget_usd in config.yaml (restart to apply), then Retry", spent, budget)
}

// Timed-park delays runAgent stamps on a transient agent park (see scheduler
// auto-readmit): the scheduler re-admits the book once the instant passes.
const (
	// rateLimitReadmitBuffer is added to a PARSED reset instant so the auto-readmit lands
	// safely after the limit window has fully cleared, not exactly on the boundary.
	rateLimitReadmitBuffer = 2 * time.Minute
	// rateLimitFallbackDelay is the auto-readmit delay when the backend gave no parseable
	// reset time - a conservative wait that lets most short windows clear.
	rateLimitFallbackDelay = 30 * time.Minute
	// rateLimitMinDelay is the FLOOR under a parsed-reset readmit: never schedule earlier
	// than now+5min. A reset instant that parses in the past or barely ahead (stale message,
	// timezone skew on the clock form) would otherwise readmit the book into an immediate
	// re-park, a tight loop; the floor bounds that harm to an extra short wait.
	rateLimitMinDelay = 5 * time.Minute
	// notAvailableReadmitDelay is the auto-readmit delay for an agent-unavailable park
	// (reached only after the agent package's in-process NotAvailable retries): long
	// enough that a CLI mid-reinstall is likely back, short enough to self-heal an
	// overnight batch.
	notAvailableReadmitDelay = 10 * time.Minute
)

// rateLimitRetryAt computes the auto-readmit instant for a rate-limit park: the backend's
// parsed reset time plus a buffer when known (floored at now+rateLimitMinDelay so a stale or
// tz-skewed reset can never schedule a past/immediate readmit into a re-park loop), else a
// fixed fallback from now (already well above the floor).
func rateLimitRetryAt(rl *agent.RateLimitError, now time.Time) time.Time {
	if !rl.ResetAt.IsZero() {
		at := rl.ResetAt.Add(rateLimitReadmitBuffer)
		if floor := now.Add(rateLimitMinDelay); at.Before(floor) {
			return floor
		}
		return at
	}
	return now.Add(rateLimitFallbackDelay)
}

// QANoConvergeMsg is the park reason when QA adjudication hits the hard round cap
// (maxQARounds) without converging - the backstop for a book that makes real progress
// every round (the cheaper stall park, QAStalledPrefix, handles a genuinely-stuck book
// after ~2 rounds). Built from maxQARounds so the message can never drift from the cap.
var QANoConvergeMsg = fmt.Sprintf("QA adjudication did not converge after %d rounds - see qa_report.md", maxQARounds)

// ensureAgent re-selects an agent runner when the frozen one is unavailable, adopting
// a now-available result for this and future runs (the operator installs a CLI, then
// Retry). Detect is cheap (PATH/stat + a fast --version). It mirrors ensureASR.
func (e *Executor) ensureAgent(ctx context.Context) (agent.Runner, agent.Availability) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.agentRun != nil && e.agentAvail.Available {
		return e.agentRun, e.agentAvail
	}
	if r, av := e.redetectAgent(ctx); r != nil && av.Available {
		e.agentRun, e.agentAvail = r, av
	}
	return e.agentRun, e.agentAvail
}

// defaultRedetectAgent re-runs agent.Select and reports a usable runner (or nil when
// none is available). It is the production redetectAgent.
func (e *Executor) defaultRedetectAgent(ctx context.Context) (agent.Runner, agent.Availability) {
	r, av, _ := agent.Select(ctx, e.agentSelect, e.secrets)
	return r, av
}

// AgentStatus returns the executor's current agent-runner availability (which a stage
// may have re-detected after a retry), for /system. Safe for concurrent use.
func (e *Executor) AgentStatus() agent.Availability {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.agentAvail
}

// ActivateFallback adopts a pre-approved backend/model for subsequent agent
// invocations. Only the supervisor wiring calls this, and only when the explicit
// allow_backend_failover configuration gate is enabled.
func (e *Executor) ActivateFallback(ctx context.Context, backend, model string) error {
	sel := e.agentSelect
	sel.Backend = backend
	r, av, err := agent.Select(ctx, sel, e.secrets)
	if err != nil {
		return err
	}
	if r == nil || !av.Available {
		return fmt.Errorf("fallback backend %q unavailable: %s", backend, av.Detail)
	}
	e.mu.Lock()
	e.agentRun, e.agentAvail, e.agentSelect = r, av, sel
	if model != "" {
		if e.agentModels.Claude == nil {
			e.agentModels.Claude = map[string]string{}
		}
		if e.agentModels.OpenAI == nil {
			e.agentModels.OpenAI = map[string]string{}
		}
		for _, st := range state.All() {
			if !state.IsAgent(st) {
				continue
			}
			if backend == agent.IDClaude {
				e.agentModels.Claude[string(st)] = model
			} else {
				e.agentModels.OpenAI[string(st)] = model
			}
		}
	}
	e.mu.Unlock()
	return nil
}

// agentUsage is the accumulated token/cost spend of all invocations in one agent
// stage run plus the invocation count and the productive agent seconds, returned by
// runAgent so a stage can fold it into its metrics and its StageResult.RateSample.
type agentUsage struct {
	agent.Usage
	Invocations int
	// Seconds is the productive agent wall-time this run: the time spent in the agent
	// invocations (validation retries included) with rate-limit backoff sleep excluded,
	// so it feeds a per-stage rate that reflects the model's work, not waiting.
	Seconds float64
}

// rateSample builds the one-shot (whole-book) rate sample for an agent stage: 1 unit
// spent in Seconds productive seconds, or nil when no agent work ran (e.g. an all-auto
// qa_adjudicating pass) so the scheduler records nothing.
func (u agentUsage) rateSample() *scheduler.RateSample {
	return rateSample(1, u.Seconds)
}

// add folds one invocation's usage into the running total (the six spend fields plus
// the last non-empty model). Callers own the Invocations count - runAgent's onUsage
// bumps it by one per invocation, a multi-invocation stage sums the per-run totals -
// because agent.Usage itself carries no invocation count.
func (u *agentUsage) add(x agent.Usage) {
	u.Input += x.Input
	u.Output += x.Output
	u.CacheRead += x.CacheRead
	u.CostUSD += x.CostUSD
	reported := x.CostReported || x.CostUSD > 0
	if u.Invocations == 0 {
		u.CostReported = reported
	} else {
		u.CostReported = u.CostReported && reported
	}
	u.Turns += x.Turns
	if x.Model != "" {
		u.Model = x.Model
	}
}

// metricsMap renders the usage summary for a stage's StageResult.Metrics.
func (u agentUsage) metricsMap() map[string]any {
	return map[string]any{
		"usage": map[string]any{
			"model":         u.Model,
			"input_tokens":  u.Input,
			"output_tokens": u.Output,
			"cache_read":    u.CacheRead,
			"cost_usd":      u.CostUSD,
			"cost_reported": u.CostReported,
			"turns":         u.Turns,
			"invocations":   u.Invocations,
		},
	}
}

// stageMaxTurns bounds the CLI's internal tool loop by workload. The old global 200
// turn ceiling let malformed file-navigation plans run for tens of minutes. These
// limits still allow several reads per staged file plus validation-retry patches, but
// fail a runaway invocation before it can dominate a book's budget.
func stageMaxTurns(stage state.State) int {
	switch stage {
	case state.MarkersNormalizing:
		return 16
	case state.QAAdjudicating:
		return 64
	case state.SpellingResearch:
		return 80
	case state.FactPass, state.Synthesizing, state.Auditing, state.Fixing:
		return 32
	default:
		return 32
	}
}

// validationError wraps a stage validator's failure so runAgent can distinguish an
// exhausted-after-retries output-validation failure (park, naming the artifact) from
// a backend/transport error (plain error) - RunWithRetry returns the validator's
// error verbatim when the retry budget is spent, so the wrapper survives to the
// errors.As check.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

// runAgent is the shared driver every agent stage uses: ensure a runner, resolve the
// stage model, render the prompt, run it (with the agent package's invalid-output +
// rate-limit retry policy), and translate the outcome into the pipeline's park/fail
// vocabulary. It captures usage into the open stage_run after EVERY invocation
// (crash preserves spend) and returns the accumulated usage so the stage can fold it
// into its metrics. validate reads the agent's out/ files from st and returns a
// non-nil error to trigger a retry; when the retry budget is spent the stage parks.
//
// Errors are translated: a rate-limited backend and an unavailable backend PARK
// (actionable, Retry-able); an exhausted output validator PARKS naming why; any other
// error (render, transport, timeout) is returned as a plain error (StatusFailed).
func (e *Executor) runAgent(ctx context.Context, book store.Book, stage state.State, r scheduler.StageReport, st *agent.Staging, promptName string, promptData any, web bool, validate func(agent.Result, *agent.Staging) error) (agentUsage, error) {
	// Per-book budget preflight: park (with everything already recorded) BEFORE spending
	// another invocation once the book's summed agent cost has reached the budget. It runs
	// per runAgent call, so fact_pass (one runAgent per chunk) naturally checks between
	// chunks; a single invocation is never aborted mid-flight. The budget sum prefers
	// provider cost and falls back to complete configured API-equivalent estimates, and
	// includes superseded rows, so a Retry never lowers the spend the guard sees.
	if e.db != nil && e.bookBudgetUSD > 0 {
		spent, complete, serr := e.db.SumStageRunBudgetCost(ctx, book.ID)
		if serr != nil {
			return agentUsage{}, fmt.Errorf("%s: read book cost: %w", stage, serr)
		}
		if !complete {
			e.log.Warn("agent: book budget has unpriced usage; known subtotal is not treated as the complete cost",
				"book", book.ID, "known_usd", spent, "pricing_version", e.pricing.Version)
		}
		if spent >= e.bookBudgetUSD {
			return agentUsage{}, scheduler.ParkWithCode(state.ParkBudgetExceeded, budgetExceededMsg(spent, e.bookBudgetUSD))
		}
	}
	runner, av := e.ensureAgent(ctx)
	if runner == nil || !av.Available {
		// PREFLIGHT unavailability: no CLI is configured/resolvable at all (not a transient
		// mid-run vanish). Park for a HUMAN with NO auto-readmit - a daemon that will never
		// have a backend until someone configures one must not churn a re-admit every 10min
		// forever. The operator installs/configures a CLI and clicks Retry. (The transient
		// case - a CLI that existed and vanished mid-run - is the POST-invocation
		// NotAvailableError branch below, which does schedule a short auto-readmit.)
		return agentUsage{}, scheduler.ParkWithCode(state.ParkAgentUnavailable, AgentUnavailableMsg)
	}
	e.mu.Lock()
	model := agent.ModelFor(e.agentModels.Claude, e.agentModels.OpenAI, runner.ID(), string(stage))
	e.mu.Unlock()

	prompt, err := prompts.Render(promptName, promptData)
	if err != nil {
		return agentUsage{}, fmt.Errorf("%s: render prompt: %w", stage, err)
	}

	var total agentUsage
	onUsage := func(u agent.Usage) {
		total.add(u)
		total.Invocations++
		if e.db != nil {
			// context.WithoutCancel: the invocation already happened, so record its spend
			// even if ctx is being cancelled - crash/cancel must not lose the accounting.
			estimated, estimatedKnown := e.pricing.Estimate(runner.ID(), u.Model, u.Input, u.Output, u.CacheRead)
			providerReported := u.CostReported || u.CostUSD > 0
			if uerr := e.db.AddOpenStageRunUsageDetailed(context.WithoutCancel(ctx), book.ID, string(stage),
				u.Model, u.Input, u.Output, u.CacheRead, u.CostUSD, providerReported, estimated, estimatedKnown); uerr != nil {
				e.log.Warn("agent: record usage", "book", book.ID, "stage", string(stage), "err", uerr)
			}
		}
	}

	req := agent.Request{
		Stage:    string(stage),
		Dir:      st.Dir(),
		Prompt:   prompt,
		Model:    model,
		Web:      web,
		Timeout:  e.agentTimeout,
		MaxTurns: stageMaxTurns(stage),
		// Liveness heartbeat: while the agent subprocess runs, emit a durable note so a
		// long stage (a 6-minute qa_adjudicating) visibly proves the daemon is alive. It
		// fires only while the child is running (never during rate-limit backoff).
		Heartbeat: func(elapsed time.Duration) {
			if e.db != nil {
				_ = e.db.TouchOpenStageRun(context.WithoutCancel(ctx), book.ID, string(stage), false)
			}
			if r.Note != nil {
				r.Note(fmt.Sprintf("%s: still running (%s elapsed)", stage, humanDuration(elapsed)))
			}
		},
		Process: func(pid int, active bool) {
			if e.db != nil {
				_ = e.db.SetOpenStageRunProcess(context.WithoutCancel(ctx), book.ID, string(stage), pid, active)
			}
		},
	}
	backoff := e.backoff
	if backoff == nil {
		backoff = agent.DefaultBackoff()
	}
	// The scheduler caps concurrent agent stages, while fact_pass can additionally
	// run independent chunk invocations in parallel. Share one executor-level
	// semaphore across both shapes so the configured concurrency remains a real cap,
	// not a multiplier. Cancellation while queued spends nothing.
	select {
	case e.agentSlots <- struct{}{}:
		defer func() { <-e.agentSlots }()
	case <-ctx.Done():
		return total, ctx.Err()
	}
	// Queueing behind another invocation is capacity wait, not productive model
	// time. Start the rate clock only after this invocation owns a slot.
	start := time.Now()
	_, slept, err := agent.RunWithBackoff(ctx, runner, req, func(res agent.Result) error {
		if verr := validate(res, st); verr != nil {
			return &validationError{msg: verr.Error()}
		}
		return nil
	}, onUsage, backoff)
	// Charge only productive agent time to the rate: wall-clock minus the rate-limit
	// backoff sleep RunWithRetry reports (validation retries stay counted - they are
	// genuine model cost - only waiting out a rate limit is excluded).
	if productive := time.Since(start) - slept; productive > 0 {
		total.Seconds = productive.Seconds()
	}
	if err != nil {
		var rl *agent.RateLimitError
		if errors.As(err, &rl) {
			// Schedule an automatic re-admit shortly after the limit window clears (the
			// backend's parsed reset time + a buffer, else a fixed fallback) so an overnight
			// batch heals itself instead of stranding until a human clicks Retry.
			return total, scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited,
				AgentRateLimitedPrefix+" ("+rl.Detail+") - retry later", rateLimitRetryAt(rl, time.Now()))
		}
		var na *agent.NotAvailableError
		if errors.As(err, &na) {
			// Reached only after the agent package rode out the transient LookPath blip, so a
			// short auto-readmit window lets a CLI that finished (re)installing self-resume.
			return total, scheduler.ParkWithCodeAfter(state.ParkAgentUnavailable, AgentUnavailableMsg,
				time.Now().Add(notAvailableReadmitDelay))
		}
		var ve *validationError
		if errors.As(err, &ve) {
			return total, scheduler.ParkWithCode(state.ParkAgentValidationExhausted, AgentValidationExhaustedPrefix+": "+ve.msg)
		}
		return total, fmt.Errorf("%s: agent run: %w", stage, err)
	}
	return total, nil
}

// humanDuration renders d as a compact whole-unit elapsed string for a liveness note:
// "45s" under a minute, "6m" for whole minutes, "1h2m" past an hour. Precision beyond
// whole minutes/seconds is noise for a "still running" heartbeat.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%dh%dm", h, m)
}

// countNoun renders a count with a naively pluralized noun for a human note:
// countNoun(1, "chunk") -> "1 chunk", countNoun(3, "chunk") -> "3 chunks". Adequate
// for the pipeline's simple nouns (chapter/chunk); it does not handle irregulars.
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// stageAttempt is a per-stage attempt number used only to name the staged dir
// (_runs/<stage>-a<NN>) so successive scheduler re-runs keep distinct debug dirs. It
// derives from the stage's run count (the scheduler has already opened the current
// run, so this is that run's attempt); it degrades to 1 with no db or on error.
func (e *Executor) stageAttempt(ctx context.Context, book store.Book, stage state.State) int {
	if e.db == nil {
		return 1
	}
	n, err := e.db.CountStageRuns(ctx, book.ID, string(stage))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// --- markers_normalizing (agent, Lane B, Web=false) ---

// markersPromptData feeds markers.md. Field names MUST match the template (rendered
// with missingkey=error, so a drift fails loudly at render time).
type markersPromptData struct {
	Title        string
	Authors      string
	Series       string
	SeriesPos    string
	Style        string
	Duration     float64
	ChapterCount int
}

// markerVerdict is the agent's confidence signal (out/verdict.json): a not-confident
// verdict parks the book for a human rather than adopting a guessed mapping.
type markerVerdict struct {
	Confident bool   `json:"confident"`
	Reason    string `json:"reason"`
}

// markersNormalize maps a non-contiguous recording's raw markers to logical work
// chapters via the agent, replacing manifest.json with a validated contiguous map. A
// not-confident verdict parks the book needs_attention (a human decision point, not a
// failure); an unavailable agent parks with AgentUnavailableMsg.
func (e *Executor) markersNormalize(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	draft, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: read manifest (inspect must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("normalizing markers over %s", countNoun(draft.ChapterCount, "chapter")))
	}
	st, err := agent.New(book.WorkDir, string(state.MarkersNormalizing), e.stageAttempt(ctx, book, state.MarkersNormalizing))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := st.CopyFile(filepath.Join(book.WorkDir, audio.ProbeName), audio.ProbeName); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: stage probe.json: %w", err)
	}
	if err := st.CopyFile(filepath.Join(book.WorkDir, audio.ManifestName), audio.ManifestName); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: stage manifest.json: %w", err)
	}

	inputPaths := make(map[string]bool, len(draft.Chapters))
	for _, ch := range draft.Chapters {
		if ch.FilePath != "" {
			inputPaths[ch.FilePath] = true
		}
	}
	validate := func(_ agent.Result, s *agent.Staging) error {
		// A not-confident verdict is a VALID terminal output: the agent followed the
		// prompt's "do not guess" instruction and declined to invent a mapping, so it may
		// legitimately have written no (or a partial) out/manifest.json. Accept it without
		// a retry and let the post-run path park with its reason. Only a CONFIDENT verdict
		// must satisfy the full manifest contract.
		v, verr := readMarkerVerdict(s.OutDir())
		if verr != nil {
			return fmt.Errorf("out/verdict.json: %v", verr)
		}
		if !v.Confident {
			return nil
		}
		return validateMarkersManifest(s.OutDir(), draft, inputPaths)
	}
	data := markersPromptData{
		Title:        book.Title,
		Authors:      authors(book),
		Series:       book.Series,
		SeriesPos:    book.SeriesPos,
		Style:        draft.Style,
		Duration:     draft.Duration,
		ChapterCount: draft.ChapterCount,
	}
	usage, err := e.runAgent(ctx, book, state.MarkersNormalizing, r, st, "markers.md", data, false, validate)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	// A successful agent run that reports it could not produce a confident mapping is
	// a human decision point, not a failure: park needs_attention (do NOT harvest the
	// draft-quality manifest over the original).
	verdict, err := readMarkerVerdict(st.OutDir())
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: read verdict: %w", err)
	}
	if !verdict.Confident {
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "the agent could not produce a confident marker mapping"
		}
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMarkersNotConfident, MarkersNotConfidentPrefix+": "+reason)
	}

	if err := agent.Harvest(st, []agent.HarvestSpec{{From: audio.ManifestName, To: audio.ManifestName}}); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("markers_normalizing: harvest manifest: %w", err)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	result := scheduler.StageResult{Metrics: metrics(usage.metricsMap()), RateSample: usage.rateSample()}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.MarkersNormalizing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// validateMarkersManifest checks a CONFIDENT agent's out/manifest.json against the
// contract: it parses as an audio.Manifest, keeps the draft's Style, numbers its
// chapters uniquely/orderly/contiguously, every interval is start<end within [0,
// Duration+1s], ChapterCount matches, and its file paths are a subset of the draft's
// (the agent may only renumber/exclude/retitle, never invent an interval or file). The
// caller gates this on a confident verdict (a not-confident verdict skips the manifest
// requirement entirely and parks with its reason), so it need not re-read the verdict.
func validateMarkersManifest(outDir string, draft audio.Manifest, inputPaths map[string]bool) error {
	raw, err := os.ReadFile(filepath.Join(outDir, audio.ManifestName)) //nolint:gosec // outDir is the agent's staged out/ dir under the work dir
	if err != nil {
		return fmt.Errorf("out/manifest.json: %v", err)
	}
	var m audio.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("out/manifest.json is not valid manifest JSON: %v", err)
	}
	if m.Style != draft.Style {
		return fmt.Errorf("style changed from %q to %q - you may not change the recording layout", draft.Style, m.Style)
	}
	if len(m.Chapters) == 0 {
		return fmt.Errorf("out/manifest.json has no chapters")
	}
	if !audio.Contiguous(m.Chapters) {
		return fmt.Errorf("chapter numbers must be unique, ordered, and contiguous (1,2,3,...)")
	}
	for _, ch := range m.Chapters {
		if ch.Start >= ch.End {
			return fmt.Errorf("chapter %d has start >= end", ch.Chapter)
		}
		if ch.Start < 0 || ch.End > draft.Duration+1.0 {
			return fmt.Errorf("chapter %d interval [%.3f,%.3f] is outside the recording [0,%.3f]", ch.Chapter, ch.Start, ch.End, draft.Duration)
		}
		if ch.FilePath != "" && !inputPaths[ch.FilePath] {
			return fmt.Errorf("chapter %d file path %q is not one of the draft manifest's files", ch.Chapter, ch.FilePath)
		}
	}
	if m.ChapterCount != len(m.Chapters) {
		return fmt.Errorf("chapter_count %d does not match the %d chapters", m.ChapterCount, len(m.Chapters))
	}
	return nil
}

// readMarkerVerdict parses out/verdict.json.
func readMarkerVerdict(outDir string) (markerVerdict, error) {
	raw, err := os.ReadFile(filepath.Join(outDir, "verdict.json")) //nolint:gosec // outDir is the agent's staged out/ dir under the work dir
	if err != nil {
		return markerVerdict{}, err
	}
	var v markerVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return markerVerdict{}, err
	}
	return v, nil
}

// --- qa_adjudicating (agent, Lane B, Web=false) ---

// adjudicatePromptData feeds adjudicate.md. AutoAccepted is the comma-joined chapter
// list the pipeline already accepted mechanically (item 4) - the agent must NOT
// disposition those; the template renders an optional block naming them.
type adjudicatePromptData struct {
	Title        string
	Round        int
	AutoAccepted string
}

// autoAcceptTailReason is the fixed reason on a pipeline-authored auto-accept entry (a
// tail_clip or mid_clip splice already landed; the residual reads the untouched raw layer).
const autoAcceptTailReason = "already repaired via clip splice - splice present in transcripts-repaired"

// acceptedLedgerName is the work-dir file recording every chapter qa_adjudicating accepted
// (agent or auto) across rounds, so an accepted chapter is never re-adjudicated in a later
// round. Without it most detectors re-flag an already-accepted chapter each qa_sweep round
// (they read the stale unrepaired layer), so the agent re-opens and re-accepts the SAME
// chapters every round at full cost - the round-cap burner one real book exhausted its budget
// on. The decisions stay valid because repairs only ever touch PLANNED non-accept chapters, so
// an accepted chapter's transcript never changes afterwards; the ledger is therefore
// deliberately NOT cleared by the done==0 reset or a Retry (unlike the stall marker). A
// deleted + re-enqueued book gets a fresh work dir, so it never inherits a stale ledger.
const acceptedLedgerName = "qa_accepted.json"

// acceptedEntry records one accepted chapter's disposition: the round it was first accepted,
// the argued reason, and whether the acceptance was the agent's or a mechanical auto-accept.
type acceptedEntry struct {
	Round  int    `json:"round"`
	Reason string `json:"reason"`
	Source string `json:"source"` // "agent" | "auto"
}

// loadAcceptedLedger reads qa_accepted.json (chapter -> entry), tolerating an absent,
// unreadable, or malformed file as an empty ledger so a first round (or a corrupt artifact)
// simply starts fresh rather than failing the stage.
func loadAcceptedLedger(workDir string) map[int]acceptedEntry {
	raw, err := os.ReadFile(filepath.Join(workDir, acceptedLedgerName)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return map[int]acceptedEntry{}
	}
	var m map[int]acceptedEntry
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[int]acceptedEntry{}
	}
	return m
}

// writeAcceptedLedger persists the ledger (pretty JSON, trailing newline) atomically.
func writeAcceptedLedger(workDir string, ledger map[int]acceptedEntry) error {
	out, err := json.MarshalIndent(ledger, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, acceptedLedgerName), append(out, '\n'), 0o644)
}

// repairOutcomesName is the work-dir file recording the LATEST repair attempt outcome per
// chapter across QA rounds. It exists because tail_verdicts.json + repairs.log leave several
// terminal mechanical facts INVISIBLE to the next adjudicator: a full `retranscribe` that ran
// and was KEPT (the fresh no-context output was not adoptable, so the original stands) writes
// no repairs.log line and no verdict, and the fresh raw in retranscribe/ is not staged; a
// `skipped_known_failed` window and an `unlocatable` tail_clip are likewise not obvious from
// the staged artifacts. So the adjudicator was being asked to apply the prompt's exhaustion
// rules (a kept retranscribe -> accept; an unlocatable tail_clip -> supply clip_start_sec)
// against evidence it could not see. This file surfaces exactly that evidence.
//
// Like qa_accepted.json it records DURABLE mechanical facts and is deliberately NOT cleared on
// the done==0 reset or a Retry: a re-run under the SAME decode params reproduces the same
// outcomes, and the decode-params reconcile (ensureRetranscribeDecodeParams) already
// invalidates stale fresh raws when the params change. It is ADVISORY context for the agent,
// never a mechanical gate (nothing in the pipeline branches on it).
const repairOutcomesName = "repair_outcomes.json"

// repairOutcome records one chapter's LATEST repair attempt: the action the plan queued, the
// mechanical result it produced, the QA round it ran in, and the clip window bounds when the
// action carried them (a directed tail_clip or a mid_clip). Outcome is one of adopted | kept
// (retranscribe), spliced | redegenerated | skipped_known_failed | unlocatable (clip).
type repairOutcome struct {
	Chapter      int     `json:"chapter"`
	Action       string  `json:"action"`
	Outcome      string  `json:"outcome"`
	Round        int     `json:"round"`
	ClipStartSec float64 `json:"clip_start_sec,omitempty"`
	ClipEndSec   float64 `json:"clip_end_sec,omitempty"`
}

// loadRepairOutcomes reads repair_outcomes.json (chapter -> outcome), tolerating an absent,
// unreadable, or malformed file as an empty map so a first round (or a corrupt artifact)
// starts fresh rather than failing the stage.
func loadRepairOutcomes(workDir string) map[int]repairOutcome {
	raw, err := os.ReadFile(filepath.Join(workDir, repairOutcomesName)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return map[int]repairOutcome{}
	}
	var m map[int]repairOutcome
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[int]repairOutcome{}
	}
	return m
}

// writeRepairOutcomes persists the outcomes map (pretty JSON, trailing newline) atomically.
func writeRepairOutcomes(workDir string, outcomes map[int]repairOutcome) error {
	out, err := json.MarshalIndent(outcomes, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, repairOutcomesName), append(out, '\n'), 0o644)
}

// clipOutcomeString maps a clip repair's ClipResult to its repair_outcomes.json outcome
// string. It mirrors recordClipOutcome's bucketing (spliced / skipped_known_failed /
// unlocatable / else redegenerated) so the durable record and the metrics counters agree.
func clipOutcomeString(res repair.ClipResult) string {
	switch {
	case res.Spliced:
		return "spliced"
	case res.SkippedKnownFailed:
		return "skipped_known_failed"
	case res.Unlocatable():
		return "unlocatable"
	default:
		return "redegenerated"
	}
}

// retranscribeStalledMarker is the work-dir file the retranscribing stage writes (and
// INCREMENTS) each time a repair round made no progress (it neither spliced nor adopted
// any chapter) and removes when a round DID make progress. Its integer value is the count
// of consecutive no-progress rounds. It is the pipeline's QA-loop convergence signal:
// qaAdjudicate reads it (retranscribeStalledCount) and parks ParkQANoConverge only at
// count >= 2 - TWO consecutive no-progress rounds - rather than burning another paid agent
// round on a book the repairs cannot move. The FIRST no-progress round (count 1) still gets
// one resolution agent round, because that round produced the very feedback (unlocatable
// notes, known-failed skips) the adjudicator needs for its terminal disposition. It is not
// a pipeline sentinel and never gates the state machine.
//
// Why a progress tally and not a report/ledger fingerprint (the design this replaced): a
// book whose tail-clip repairs keep re-degenerating rewrites tail_verdicts.json every
// round - each futile CLIP-REDEGENERATED verdict carries a fresh clip_start - so a
// fingerprint over qa_report.json + the verdict ledger changed every round, the fixed
// point never fired, and the book burned its whole round budget (~$1.5 per agent round)
// before the cap. A round that splices AND adopts nothing is the true "no progress"
// signal, cheap to compute and immune to a churning ledger.
const retranscribeStalledMarker = "retranscribe_stalled"

// retranscribeStalledPath is the stall marker's path in the book's work dir.
func retranscribeStalledPath(workDir string) string {
	return filepath.Join(workDir, retranscribeStalledMarker)
}

// retranscribeStalledCount reads the stall marker's integer value: the number of
// CONSECUTIVE no-progress repair rounds. Absent -> 0. A present-but-malformed marker
// (a legacy "1\n", or any unparseable content) counts as 1 - it records at least one
// stall. qaAdjudicate parks only at count >= 2 (two consecutive no-progress rounds are
// a genuine stall); at count 1 it proceeds to one resolution agent round, because the
// first no-progress round is exactly what produces the feedback (clips_unlocatable
// notes, known-failed skips, kept retranscribes) the adjudicator needs for a terminal
// disposition (accept, or a directed window with clip_start_sec).
func retranscribeStalledCount(workDir string) int {
	raw, err := os.ReadFile(retranscribeStalledPath(workDir)) //nolint:gosec // path derives from the book's own work dir
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// qaStalledEntries resolves the stuck chapter set naming the stall park message: the prior
// round's plan's non-accept entries (the repair dispositions that keep failing). It is
// naming-only; an empty result yields a chapter-less message. The plan is always present
// and carries non-accept entries at the only call site: the stall marker is written only by
// retranscribe (which runs only for a RetranscribeNeeded plan, i.e. one with non-accept
// entries), qa_plan.json is never deleted before qaAdjudicate reads it, and the stall guard
// runs before this round writes a new plan - so the plan on disk is still that stuck round's.
func qaStalledEntries(workDir string) []qa.PlanEntry {
	plan, err := qa.LoadPlan(workDir)
	if err != nil {
		return nil
	}
	return plan.NonAcceptEntries()
}

// qaStalledMsg builds the stall park reason, naming the stuck chapters.
func qaStalledMsg(stuck []qa.PlanEntry) string {
	if len(stuck) == 0 {
		return QAStalledPrefix + " - see qa_report.md"
	}
	return fmt.Sprintf("%s; stuck chapters: %s - see qa_report.md", QAStalledPrefix, chaptersCSV(stuck))
}

// maxQARounds is the hard cap on qa_adjudicating rounds. The cheap progress-based stall
// park (retranscribeStalledMarker) now parks a genuinely-stuck book after ~2 rounds, so
// this cap only bounds a book that makes real progress every round; 5 gives incremental
// convergence more room than the old 3 without risking unbounded agent cost. It stays as
// the backstop for the pathological "makes a little progress forever" case.
const maxQARounds = 5

// qaAdjudicate hands the QA sweep's findings to the agent, which writes a qa_plan.json
// dispositioning every flagged chapter (retranscribe / tail_clip / accept). It caps at
// maxQARounds (a plan that keeps re-queuing does not converge), stages only the flagged
// chapters' transcripts, validates the plan against the report, and reports
// RetranscribeNeeded so the state machine branches to retranscribing or
// spelling_research.
//
// Before invoking the agent it AUTO-ACCEPTS every flagged chapter that a prior
// tail_clip round already repaired and whose only findings are tail-related (item 4):
// the tail_rate detector reads transcripts-json/, which a splice does not touch, so
// the hit persists on re-sweep even though the repaired layer is fixed - without this,
// convergence would depend on the agent choosing "accept", and an agent that re-picks
// tail_clip forever would park the book at the round cap despite it being repaired.
// The auto-accept is decided in the STAGE (never in the golden-tested qa detectors).
// When every flagged chapter is auto-accepted, the agent is not invoked at all.
func (e *Executor) qaAdjudicate(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	if r.Progress != nil {
		r.Progress(0, 1)
	}
	// Round cap FIRST: CountStageSuccesses is completed rounds (the current run is still
	// open, so not counted). maxQARounds completed rounds without convergence -> park.
	round := 1
	if e.db != nil {
		done, err := e.db.CountStageSuccesses(ctx, book.ID, string(state.QAAdjudicating))
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: count rounds: %w", err)
		}
		if done >= maxQARounds {
			// Round-cap park: clear the stall marker too, so ANY ParkQANoConverge at this
			// stage leaves a clean slate - a user Retry then gets one fresh round.
			_ = os.Remove(retranscribeStalledPath(book.WorkDir))
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkQANoConverge, QANoConvergeMsg)
		}
		// done==0 reset path: clear the stall marker for lifecycle parity - a Retry (which
		// resets the stage_runs rows) or a purge-rewind must not inherit a stale marker, so
		// the first round after a reset always runs the agent (the documented one-fresh-round
		// contract) instead of parking on the previous life's signal.
		if done == 0 {
			_ = os.Remove(retranscribeStalledPath(book.WorkDir))
		}
		round = done + 1
	}
	rep, err := qa.LoadReport(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: load report (qa_sweep must run first): %w", err)
	}
	// Stall guard: park only after TWO consecutive no-progress repair rounds (marker count
	// >= 2). The first no-progress round (count 1) is NOT a stall - it is the round that
	// produced the feedback (clips_unlocatable notes naming chapters that need a
	// clip_start_sec, known-failed skips, kept retranscribes) the adjudicator needs to
	// disposition the residue terminally (accept, or a directed window). So at count 1 we
	// PROCEED to one more agent round, leaving the marker in place: the next retranscribing
	// round either makes progress (removing it) or increments it to 2, and the following
	// adjudication parks. Two consecutive no-op rounds mean the repairs genuinely cannot
	// move the book. When we DO park, delete the marker so a user Retry gets a fresh loop
	// (see the done==0 reset above - cleared on both, so a Retry never inherits it).
	if retranscribeStalledCount(book.WorkDir) >= 2 {
		stuck := qaStalledEntries(book.WorkDir)
		_ = os.Remove(retranscribeStalledPath(book.WorkDir))
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkQANoConverge, qaStalledMsg(stuck))
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("adjudicating round %d: %s", round, countNoun(len(qa.FlaggedChapters(rep)), "flagged chapter")))
	}

	autoEntries := e.autoAcceptRepairedTails(rep, book.WorkDir)
	autoSet := make(map[int]bool, len(autoEntries))
	for _, en := range autoEntries {
		autoSet[en.Chapter] = true
	}
	// Fold the durable accepted-chapters ledger into the mechanical-accept set: any flagged
	// chapter accepted in a PRIOR round (agent or auto) is accepted mechanically again this
	// round and excluded from the set the agent must adjudicate. This is what stops the agent
	// re-verifying the SAME already-accepted chapters every round at full cost (the round-cap
	// burner). Folded into autoEntries/autoSet so the merged plan, the prompt's do-not-
	// disposition list, and the remaining-count all treat a ledger accept exactly like an
	// auto-accept.
	ledger := loadAcceptedLedger(book.WorkDir)
	for _, ch := range qa.FlaggedChapters(rep) {
		if autoSet[ch] {
			continue
		}
		le, ok := ledger[ch]
		if !ok {
			continue
		}
		autoEntries = append(autoEntries, qa.PlanEntry{
			Chapter: ch,
			Action:  qa.ActionAccept,
			Reason:  fmt.Sprintf("%s (accepted round %d)", le.Reason, le.Round),
		})
		autoSet[ch] = true
	}
	remaining := 0
	for _, ch := range qa.FlaggedChapters(rep) {
		if !autoSet[ch] {
			remaining++
		}
	}

	var plan *qa.Plan
	var usage agentUsage
	if remaining == 0 {
		// Every flagged chapter is auto-accepted (or the report flags nothing): an
		// all-accept plan with no agent invocation.
		plan = &qa.Plan{Entries: autoEntries}
	} else {
		p, u, aerr := e.runQAAdjudicateAgent(ctx, book, r, rep, round, autoEntries, autoSet)
		if aerr != nil {
			return scheduler.StageResult{}, aerr
		}
		plan, usage = p, u
	}

	// Persist every accept in the final (merged, validated) plan into the durable ledger -
	// agent's AND auto's - so a later round accepts them mechanically instead of re-adjudicating
	// them. An existing entry is never overwritten, so the FIRST acceptance round/reason is kept
	// and the "(accepted round N)" suffix never nests across rounds.
	for _, en := range plan.Entries {
		if en.Action != qa.ActionAccept {
			continue
		}
		if _, exists := ledger[en.Chapter]; exists {
			continue
		}
		src := "agent"
		if autoSet[en.Chapter] {
			src = "auto"
		}
		ledger[en.Chapter] = acceptedEntry{Round: round, Reason: en.Reason, Source: src}
	}
	if err := writeAcceptedLedger(book.WorkDir, ledger); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: write accepted ledger: %w", err)
	}

	if err := qa.WritePlan(book.WorkDir, plan); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("qa_adjudicating: write plan: %w", err)
	}
	if r.Progress != nil {
		r.Progress(1, 1)
	}
	m := usage.metricsMap()
	m["auto_accepted"] = len(autoEntries)
	result := scheduler.StageResult{
		RetranscribeNeeded: plan.RetranscribeNeeded(),
		Metrics:            metrics(m),
		// nil when every flagged chapter was auto-accepted (no agent invocation ran), so
		// a zero-work adjudication records no rate.
		RateSample: usage.rateSample(),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.QAAdjudicating), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// runQAAdjudicateAgent stages the report + flagged transcripts, runs the agent, and
// returns the MERGED plan (the daemon's auto-accept entries plus the agent's
// dispositions for the remaining chapters) validated against the report. The merge is
// what lets the plan validator require the agent to cover only the non-auto chapters
// while the persisted plan still covers every flagged chapter.
func (e *Executor) runQAAdjudicateAgent(ctx context.Context, book store.Book, r scheduler.StageReport, rep *qa.Report, round int, autoEntries []qa.PlanEntry, autoSet map[int]bool) (*qa.Plan, agentUsage, error) {
	st, err := agent.New(book.WorkDir, string(state.QAAdjudicating), e.stageAttempt(ctx, book, state.QAAdjudicating))
	if err != nil {
		return nil, agentUsage{}, err
	}
	// Required inputs.
	for _, name := range []string{qa.ReportJSONName, qa.ReportMDName, audio.ManifestName} {
		if err := st.CopyFile(filepath.Join(book.WorkDir, name), name); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", name, err)
		}
	}
	// Optional re-entry artifacts (present only on rounds > 1). repair_outcomes.json is the
	// one that surfaces the OTHERWISE-INVISIBLE terminal facts (a kept retranscribe, a
	// known-failed skip, an unlocatable tail_clip) the adjudicator's exhaustion rules need.
	for _, name := range []string{qa.PlanFile, repair.TailVerdictsName, repair.RepairsLogName, repairOutcomesName} {
		if err := e.stageIfPresent(st, book.WorkDir, name, name); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", name, err)
		}
	}
	// The ALLOWED chapters' transcript text (and any repaired copy) - the disposition
	// surface (every chapter the plan validator lets the agent disposition), NOT just the
	// required/flagged subset. The agent may verify and accept an allowed-but-not-flagged
	// chapter (a short end-fade repeat, a cross-segment residual) against its real text
	// rather than blind-queueing a conservative tail_clip; these are still the ONLY
	// transcripts the stage stages, so a chapter the sweep did not flag at all never leaks.
	for _, ch := range qa.AllowedChapters(rep) {
		rel := filepath.Join(transcript.TextDir, transcript.TextName(ch))
		if err := e.stageIfPresent(st, book.WorkDir, rel, rel); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", rel, err)
		}
		relRep := filepath.Join(transcript.RepairedDir, transcript.TextName(ch))
		if err := e.stageIfPresent(st, book.WorkDir, relRep, relRep); err != nil {
			return nil, agentUsage{}, fmt.Errorf("qa_adjudicating: stage %s: %w", relRep, err)
		}
	}

	// Capture the validated MERGED plan from the successful attempt.
	var merged *qa.Plan
	validate := func(_ agent.Result, s *agent.Staging) error {
		p, perr := qa.LoadPlan(s.OutDir())
		if perr != nil {
			return perr
		}
		mp := mergePlans(autoEntries, autoSet, p)
		if verr := mp.Validate(rep); verr != nil {
			return verr
		}
		merged = mp
		return nil
	}
	data := adjudicatePromptData{Title: book.Title, Round: round, AutoAccepted: chaptersCSV(autoEntries)}
	usage, err := e.runAgent(ctx, book, state.QAAdjudicating, r, st, "adjudicate.md", data, false, validate)
	if err != nil {
		return nil, usage, err
	}
	return merged, usage, nil
}

// autoAcceptRepairedTails returns an accept entry for every flagged chapter whose only
// findings are tail-related (or tail-residuals the chapter's splice covers) AND which a
// prior tail_clip round already repaired (both transcripts-repaired/<ch>.txt and a
// tail_verdicts.json entry present). The result is deterministic (FlaggedChapters is
// sorted). It loads the verdict ledger ONCE and uses that same map for both the
// tail-residual classification (tailOnlyChapters reads each verdict's ClipStart) and the
// ledger-presence half of the repaired-evidence check (the repaired-file existence stays a
// direct fsutil.IsFile check) - so there is no per-chapter reload. An unreadable ledger
// degrades to no verdicts (only pure tail_rate/end_fade chapters could qualify, but none
// then has the required ledger entry, so nothing auto-accepts - conservative).
func (e *Executor) autoAcceptRepairedTails(rep *qa.Report, workDir string) []qa.PlanEntry {
	byCh, err := repair.TailVerdictsByChapter(workDir)
	if err != nil {
		byCh = nil
	}
	tailOnly := tailOnlyChapters(rep, byCh)
	var out []qa.PlanEntry
	for _, ch := range qa.FlaggedChapters(rep) {
		if !tailOnly[ch] {
			continue
		}
		// Repaired evidence (the tailClipAlreadyDone pair): a splice wrote the repaired text
		// AND the ledger carries a verdict for the chapter (both from byCh, loaded once).
		if _, ok := byCh[ch]; !ok {
			continue
		}
		if !fsutil.IsFile(filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(ch))) {
			continue
		}
		out = append(out, qa.PlanEntry{Chapter: ch, Action: qa.ActionAccept, Reason: autoAcceptTailReason})
	}
	return out
}

// tailZone thresholds for classifying a cross-segment / multi-loop finding as a residual
// the chapter's recorded splice window already covers.
const (
	// tailZoneEpsilon is the slack (seconds) around a chapter's recorded splice window
	// within which a hit still counts as inside the window the splice replaced. One real
	// case: a cross-segment hit spanning 814-845s against a clip_start of 826.1.
	tailZoneEpsilon = 15.0
	// tailZonePctFloor is the position-percent tail floor used when a hit carries no
	// usable segment time (the report's "-1.0% (?)" entries) AND the window is a TAIL
	// window: at or above it the hit is in the tail zone; below it (a not-located -1
	// included) it disqualifies. A MID window never uses this fallback (conservative).
	tailZonePctFloor = 95.0
)

// tailOnlyChapters is the set of flagged (required-disposition) chapters whose findings
// are all addressable by a clip splice: a tail_rate hit, a benign end_fade run, or a
// cross-segment / tail-classified multi-loop finding that is itself a RESIDUAL the
// chapter's recorded splice window covers. The window is [clip_start, windowEnd] where
// windowEnd is the verdict's ClipEnd for a MID splice (a bounded interior window) or the
// chapter end (unbounded above) for a TAIL splice; a finding is covered when its whole
// located span sits within [clip_start - epsilon, windowEnd + epsilon] (or, for a tail
// window with no usable time, its position is in the tail >= 95%). A chapter carrying any
// wph outlier, any within-segment hit, any non-end-fade run, or any cross/multi finding
// NOT window-covered is disqualified. A MID-CHAPTER multi-loop is disqualified UNLESS it
// is covered by a recorded MID window for the chapter (a tail window never covers it).
// verdicts maps a chapter to its recorded tail_verdicts entry (ClipStart/ClipEnd are read
// for the residual test); a chapter with no entry cannot have a covered residual. It reads
// the report + verdicts only; it never touches the golden-tested qa detectors.
func tailOnlyChapters(rep *qa.Report, verdicts map[int]repair.TailVerdict) map[int]bool {
	disq := map[int]bool{}
	for _, o := range rep.WPHOutliers {
		disq[o.Chapter] = true
	}
	for _, r := range rep.RepeatedRuns {
		if r.Kind != qa.KindEndFade {
			disq[r.Chapter] = true
		}
	}
	for _, h := range rep.WithinSegment {
		disq[h.Chapter] = true
	}
	for _, h := range rep.CrossSegment {
		if v, ok := verdicts[h.Chapter]; !ok || !crossHitTailCovered(h, v) {
			disq[h.Chapter] = true
		}
	}
	for _, f := range rep.MultiLoop {
		if v, ok := verdicts[f.Chapter]; !ok || !multiLoopTailCovered(f, v) {
			disq[f.Chapter] = true
		}
	}
	out := map[int]bool{}
	for _, ch := range qa.FlaggedChapters(rep) {
		if !disq[ch] {
			out[ch] = true
		}
	}
	return out
}

// coverWindowEnd is the upper bound of a recorded verdict's coverage window: the mid
// window's clip_end when set (> 0), else +Inf for a TAIL window. A tail splice runs to the
// chapter end and every located hit time falls within the chapter, so an unbounded upper
// limit is exactly equivalent to the chapter end - and avoids plumbing per-chapter
// durations the QA report does not carry.
func coverWindowEnd(v repair.TailVerdict) float64 {
	if v.IsMidWindow() {
		return v.ClipEnd
	}
	return math.Inf(1)
}

// spanCovered is the shared residual-coverage rule: a finding's located span [startSec,
// endSec] is covered by the recorded splice window when its START sits at or past
// clip_start - epsilon (the WHOLE span begins inside the window, so a hit that straddles
// mid-chapter into the tail is NOT covered) AND its END sits at or before windowEnd +
// epsilon. For a TAIL window windowEnd is +Inf, so only the lower bound constrains -
// preserving the pre-mid-clip behavior exactly. When the span has no located time
// (startSec == nil) only a tail window falls back to the position floor; a mid window is
// conservative (an untimed hit is not covered). crossHitTailCovered / multiLoopTailCovered
// delegate here.
func spanCovered(startSec, endSec *float64, pos float64, v repair.TailVerdict) bool {
	end := coverWindowEnd(v)
	if startSec == nil {
		return math.IsInf(end, 1) && pos >= tailZonePctFloor
	}
	spanEnd := *startSec
	if endSec != nil {
		spanEnd = *endSec
	}
	return *startSec >= v.ClipStart-tailZoneEpsilon && spanEnd <= end+tailZoneEpsilon
}

// crossHitTailCovered reports whether a cross-segment hit is a residual covered by the
// chapter's recorded splice window v. A CrossSegmentHit sets FirstSec/LastSec as a pair
// (Pos derives from FirstSec), so the located span is [FirstSec, LastSec] and spanCovered
// applies. For a MID window both bounds constrain; for a TAIL window only the start.
func crossHitTailCovered(h qa.CrossSegmentHit, v repair.TailVerdict) bool {
	return spanCovered(h.FirstSec, h.LastSec, h.Pos, v)
}

// multiLoopTailCovered reports whether a multi-loop finding is a residual covered by the
// chapter's recorded splice window v. A MID-CHAPTER loop overwrote interior narration, so
// only a recorded MID window (IsMidWindow) can cover it - a tail window never does.
// Otherwise the shared spanCovered rule applies to its single located time: a multi-loop
// finding carries no independent end time, so it passes a nil end (spanCovered defaults the
// span end to the start).
func multiLoopTailCovered(f qa.MultiLoopFinding, v repair.TailVerdict) bool {
	if f.MidChapter && !v.IsMidWindow() {
		return false
	}
	return spanCovered(f.AtSec, nil, f.Pos, v)
}

// mergePlans combines the daemon's auto-accept entries with the agent's plan, dropping
// any agent entry for a chapter the daemon already auto-accepted (the auto disposition
// wins). The agent's free-text notes are preserved.
func mergePlans(auto []qa.PlanEntry, autoSet map[int]bool, agentPlan *qa.Plan) *qa.Plan {
	merged := &qa.Plan{Notes: agentPlan.Notes}
	merged.Entries = append(merged.Entries, auto...)
	for _, en := range agentPlan.Entries {
		if autoSet[en.Chapter] {
			continue
		}
		merged.Entries = append(merged.Entries, en)
	}
	return merged
}

// chaptersCSV renders the chapter numbers of a plan-entry slice as a "1, 3, 7" string
// (empty for no entries), for the adjudicate prompt's auto-accepted block. It projects
// the entries to their chapter numbers and delegates to intsCSV (one shared join).
func chaptersCSV(entries []qa.PlanEntry) string {
	ns := make([]int, len(entries))
	for i, en := range entries {
		ns[i] = en.Chapter
	}
	return intsCSV(ns)
}

// intsCSV renders a slice of chapter numbers as a "12, 81" string (empty for none), for the
// unlocatable-chapters stage note.
func intsCSV(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// stageIfPresent copies src (relative to workDir) into the staged dir at rel only when
// it exists, so optional inputs are skipped cleanly.
func (e *Executor) stageIfPresent(st *agent.Staging, workDir, srcRel, dstRel string) error {
	src := filepath.Join(workDir, srcRel)
	if !fsutil.IsFile(src) {
		return nil
	}
	return st.CopyFile(src, dstRel)
}

// --- retranscribing (Lane A, MECHANICAL) ---

// retranscribe executes the qa_plan.json: full-chapter re-transcription (with an
// adoption plausibility check) for "retranscribe" entries, tail-clip repair for
// "tail_clip" entries, and nothing for "accept". It is mechanical (no agent): it
// reuses the ASR backend exactly like asrStage (prompt-free), ffmpeg for clip cuts,
// and internal/repair for the adopt/splice decisions. Re-entering qa_sweep afterwards
// re-runs the sweep (advance() clears that sentinel), so a still-dirty book loops back
// to adjudication.
func (e *Executor) retranscribe(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	plan, err := qa.LoadPlan(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("retranscribing: load plan (qa_adjudicating must run first): %w", err)
	}
	if r.Note != nil {
		r.Note(retranscribeNote(plan))
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("retranscribing: read manifest: %w", err)
	}
	durations := make(map[int]float64, len(manifest.Chapters))
	for _, ch := range manifest.Chapters {
		durations[ch.Chapter] = ch.Duration
	}

	setup, err := e.readyASR(ctx)
	if err != nil {
		return scheduler.StageResult{}, err
	}

	// Resolve the clip cutter only if the plan needs one (a tail_clip OR mid_clip entry -
	// both cut an audio window). A test injects e.clipCutter; production uses ffmpeg.
	cut := e.clipCutter
	if cut == nil && (planHasAction(plan, qa.ActionTailClip) || planHasAction(plan, qa.ActionMidClip)) {
		ffmpeg, _ := e.ensureTools()
		if ffmpeg == "" {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
		}
		cut = repair.FFmpegClipCutter(ffmpeg)
	}

	// Reconcile the retranscribe/ decode-params marker BEFORE the resume-reuse checks below:
	// a stale fresh raw produced under the old (context-conditioned) params must be dropped so
	// rawComplete cannot adopt it and deny the chapter its NoContext re-transcription. Only
	// needed when the plan actually re-transcribes a chapter (a tail_clip-only plan touches no
	// retranscribe raws).
	if planHasAction(plan, qa.ActionRetranscribe) {
		if err := ensureRetranscribeDecodeParams(book.WorkDir, retranscribeDecodeTag); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("retranscribing: reconcile decode params: %w", err)
		}
	}

	// Load the verdict ledger ONCE for the whole pass and thread the snapshot through every
	// resume-idempotency check (retranscribeEntryDone / tailClipAlreadyDone, which each used
	// to re-read tail_verdicts.json - up to three disk reads per clip entry). One pass's
	// entries touch disjoint chapters and each entry's resume check runs BEFORE its own
	// splice writes a verdict, so a top-of-pass snapshot is correct even though a splice
	// mutates the on-disk ledger mid-pass. (The repair layer's own known-failed skip still
	// reads disk inside ClipAndSplice*, so it never sees a stale snapshot.) A genuinely
	// unreadable ledger fails the stage loudly.
	verdicts, err := repair.TailVerdictsByChapter(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("retranscribing: load verdict ledger: %w", err)
	}

	// The durable per-chapter repair-outcome record (advisory context for the next
	// adjudicator, see repairOutcomesName): load the accumulated map and this run's round.
	// CountStageSuccesses is COMPLETED retranscribing runs (the current run is still open),
	// so +1 is the round this pass runs in; it degrades to round 1 with no db or on error.
	outcomes := loadRepairOutcomes(book.WorkDir)
	round := 1
	if e.db != nil {
		if n, cerr := e.db.CountStageSuccesses(ctx, book.ID, string(state.Retranscribing)); cerr == nil {
			round = n + 1
		}
	}

	// total = the work entries (every non-accept entry). completed = the work entries a
	// prior (interrupted) run already finished, so the FIRST report reflects prior work
	// and an already-repaired entry never ticks the counter on resume - the scheduler's
	// EWMA unit span then measures only the repairs THIS run actually did.
	total, completed := 0, 0
	for _, entry := range plan.Entries {
		if entry.Action == qa.ActionAccept {
			continue
		}
		total++
		if retranscribeEntryDone(book.WorkDir, entry, verdicts) {
			completed++
		}
	}
	done := completed
	if r.Progress != nil {
		r.Progress(done, total)
	}

	// Time only the repair loop (backend selection, model fetch via readyASR and the
	// clip-cutter resolution are excluded) and count only entries processed THIS run
	// (done - completed), so a resume is not charged for prior work.
	loopStart := time.Now()
	var retranscribed, adopted, kept, spliced, redegen, skippedKnownFailed, unlocatable, accepted int
	// skippedNew / unlocatableNew count free (no-ASR) outcomes processed THIS run (!wasDone):
	// a known-failed skip and a tail unlocatable no-op each tick progress (display is right to
	// advance) but did NO productive ASR work, so both are excluded from the rate sample below
	// - counting a free outcome as a processed unit would inflate the learned per-unit rate.
	skippedNew, unlocatableNew := 0, 0
	// unlocatableChapters names the chapters whose tail_clip could not be located (a short
	// repeat below the 6-gram reach) AND that carried no clip_start_sec, so the round did no
	// work on them - the stage Note asks the adjudicator to supply clip_start_sec.
	var unlocatableChapters []int
	// recordClipOutcome buckets a clip repair's ClipResult into the shared counters. Both the
	// tail and mid clip actions feed it, so a mid splice reuses the SAME buckets as a tail
	// splice - an interior repair counts as progress for the stall signal below. An unlocatable
	// no-op (only the tail path can produce it) is bucketed separately from a re-degeneration:
	// it did no ASR at all (misleadingly counting it as clips_redegenerated hid why the round
	// stalled). wasDone excludes a resumed free outcome from the rate-sample tallies.
	recordClipOutcome := func(res repair.ClipResult, wasDone bool) {
		switch {
		case res.Spliced:
			spliced++
		case res.SkippedKnownFailed:
			skippedKnownFailed++
			if !wasDone {
				skippedNew++
			}
		case res.Unlocatable():
			unlocatable++
			if !wasDone {
				unlocatableNew++
			}
			unlocatableChapters = append(unlocatableChapters, res.Chapter)
		default:
			redegen++
		}
	}
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return scheduler.StageResult{}, err // clean pause/cancel; completed chapters remain
		}
		if entry.Action == qa.ActionAccept {
			accepted++
			continue
		}
		// Capture whether this entry was already done BEFORE processing it (processing
		// makes the predicate true), so a resumed entry re-runs idempotently but does not
		// re-tick progress past the baseline.
		wasDone := retranscribeEntryDone(book.WorkDir, entry, verdicts)
		// oc records this entry's LATEST mechanical outcome for the durable repair_outcomes
		// map (upsert by chapter, written after the loop). Every processed non-accept entry
		// sets it - including a resumed (wasDone) entry, whose executor re-runs idempotently
		// and re-derives the same outcome - so the map always reflects the current attempt.
		oc := repairOutcome{Chapter: entry.Chapter, Action: string(entry.Action), Round: round}
		switch entry.Action {
		case qa.ActionRetranscribe:
			ok, rerr := e.retranscribeChapter(ctx, setup, book, durations[entry.Chapter], entry.Chapter)
			if rerr != nil {
				return scheduler.StageResult{}, rerr
			}
			retranscribed++
			if ok {
				adopted++
				oc.Outcome = "adopted"
			} else {
				kept++
				oc.Outcome = "kept"
			}
		case qa.ActionTailClip:
			res, rerr := e.tailClipChapter(ctx, setup, cut, book, durations[entry.Chapter], entry.Chapter, entry.ClipStartSec, verdicts)
			if rerr != nil {
				return scheduler.StageResult{}, rerr
			}
			recordClipOutcome(res, wasDone)
			oc.Outcome, oc.ClipStartSec = clipOutcomeString(res), entry.ClipStartSec
		case qa.ActionMidClip:
			res, rerr := e.midClipChapter(ctx, setup, cut, book, durations[entry.Chapter], entry.Chapter, entry.ClipStartSec, entry.ClipEndSec, verdicts)
			if rerr != nil {
				return scheduler.StageResult{}, rerr
			}
			recordClipOutcome(res, wasDone)
			oc.Outcome, oc.ClipStartSec, oc.ClipEndSec = clipOutcomeString(res), entry.ClipStartSec, entry.ClipEndSec
		default:
			return scheduler.StageResult{}, fmt.Errorf("retranscribing: chapter %d has unknown action %q", entry.Chapter, entry.Action)
		}
		outcomes[entry.Chapter] = oc
		if !wasDone {
			done++
			if r.Progress != nil {
				r.Progress(done, total)
			}
		}
	}

	loopSeconds := time.Since(loopStart).Seconds()
	e.accountScratch(ctx, book)

	// Persist the durable per-chapter repair-outcome record so the next adjudicator can see
	// the terminal facts (a kept retranscribe, a known-failed skip, an unlocatable tail_clip)
	// the staged verdicts/log do not carry. total > 0 whenever the plan queued work (an
	// all-accept plan never routes here), so this always reflects this round's attempts.
	if total > 0 {
		if err := writeRepairOutcomes(book.WorkDir, outcomes); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("retranscribing: write repair outcomes: %w", err)
		}
	}

	// Visibility: name the chapters whose tail_clip could not be located and carried no
	// clip_start_sec, so the book log shows why the round did no work on them - the mechanical
	// 6-gram locator cannot reach a short repeat, and the adjudicator's recourse is to re-queue
	// with an explicit clip_start_sec (or a mid_clip window).
	if len(unlocatableChapters) > 0 && r.Note != nil {
		noun := "chapters"
		if len(unlocatableChapters) == 1 {
			noun = "chapter"
		}
		r.Note(fmt.Sprintf("tail-clip could not locate a loop in %s %s - the adjudicator must supply clip_start_sec", noun, intsCSV(unlocatableChapters)))
	}

	// Convergence signal for qaAdjudicate: a repair round that neither spliced nor adopted
	// anything achieved nothing - the qa_sweep re-run will re-flag the same chapters. Persist
	// that stall by INCREMENTING the marker (the count of consecutive no-progress rounds);
	// clear it the moment a round makes real progress. qaAdjudicate parks only at count >= 2,
	// so a single no-progress round still gives the adjudicator one resolution round (its
	// unlocatable/known-failed feedback drives the next disposition) before the loop is
	// declared stuck. A mid_clip splice increments spliced (it reuses the tail buckets), so
	// an interior repair counts as progress too.
	madeProgress := spliced > 0 || adopted > 0
	if madeProgress {
		_ = os.Remove(retranscribeStalledPath(book.WorkDir))
	} else {
		next := retranscribeStalledCount(book.WorkDir) + 1
		if err := fsutil.WriteFileAtomic(retranscribeStalledPath(book.WorkDir), []byte(strconv.Itoa(next)+"\n"), 0o644); err != nil {
			return scheduler.StageResult{}, fmt.Errorf("retranscribing: write stall marker: %w", err)
		}
	}

	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"retranscribed":              retranscribed,
			"adopted":                    adopted,
			"kept":                       kept,
			"clips_spliced":              spliced,
			"clips_redegenerated":        redegen,
			"clips_skipped_known_failed": skippedKnownFailed,
			"clips_unlocatable":          unlocatable,
			"accepted":                   accepted,
		}),
		// Exclude free (no-ASR) outcomes - known-failed skips (skippedNew) AND tail unlocatable
		// no-ops (unlocatableNew) - from the productive unit count, so a round that did no real
		// ASR records no rate (rateSample returns nil for zero units) rather than inflating the
		// learned per-unit rate with ~ms no-ops.
		RateSample: rateSample(done-completed-skippedNew-unlocatableNew, loopSeconds),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Retranscribing), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// retranscribeNote renders the descriptive stage-entry line for retranscribing: which
// chapters this run will re-transcribe or tail-clip (the non-accept plan entries). An
// all-accept plan (or empty plan) says so plainly rather than naming an empty set.
func retranscribeNote(plan *qa.Plan) string {
	work := plan.NonAcceptEntries()
	if len(work) == 0 {
		return "re-transcribing: no chapters queued (all accepted)"
	}
	return fmt.Sprintf("re-transcribing %s: %s", countNoun(len(work), "chapter"), chaptersCSV(work))
}

// planHasAction reports whether any plan entry carries the given action.
func planHasAction(p *qa.Plan, a qa.PlanAction) bool {
	for _, e := range p.Entries {
		if e.Action == a {
			return true
		}
	}
	return false
}

// retranscribeEntryDone reports whether a plan entry's repair already completed on a
// prior (interrupted) run, so a resume neither re-counts it as processed work (the
// EWMA span) nor re-ticks progress past the already-done baseline. It mirrors the
// per-chapter resume tests the executors themselves use: a retranscribe entry is done
// when its fresh raw parses complete (retranscribeChapter's reuse test), a tail-clip or
// mid-clip entry when tailClipAlreadyDone finds both durable-evidence files (the same
// repaired-file + verdict pair both splice paths write). Accept entries are not work and
// never reach here.
func retranscribeEntryDone(workDir string, entry qa.PlanEntry, verdicts map[int]repair.TailVerdict) bool {
	switch entry.Action {
	case qa.ActionRetranscribe:
		return rawComplete(filepath.Join(workDir, repair.RetranscribeDir, transcript.RawName(entry.Chapter)))
	case qa.ActionTailClip, qa.ActionMidClip:
		return tailClipAlreadyDone(workDir, entry.Chapter, verdicts)
	default:
		return false
	}
}

// retranscribeDecodeTag identifies the decode parameters the repair-path re-transcription
// runs under (NoContext on, unlike the first-pass ASR). It is recorded in the retranscribe/
// decode-params marker and on every CLIP-REDEGENERATED verdict this stage writes, so a raw
// or verdict produced under DIFFERENT params (a pre-upgrade, context-conditioned run) is
// never reused to deny a chapter its one fresh NoContext attempt. Bump the suffix whenever
// the repair decode params change again.
const retranscribeDecodeTag = "nocontext-v1"

// retranscribeDecodeMarker is the file in retranscribe/ recording the decode params the
// stored fresh raws were produced under (retranscribeDecodeTag).
const retranscribeDecodeMarker = "decode_params"

// ensureRetranscribeDecodeParams reconciles the retranscribe/ dir's decode-params marker
// with the current tag ONCE at stage entry. When the marker is absent or records DIFFERENT
// params (a pre-NoContext raw), it deletes every stale fresh raw (retranscribe/*.json) so
// the resume reuse test (rawComplete) cannot adopt a raw produced under the old decode
// params, then writes the marker LAST (so a crash between the two re-clears next run). A
// matching marker is a no-op, so same-params raws are still reused across rounds/resumes
// (the intended cheap resume: cross-round reuse of same-params raws remains intentional).
func ensureRetranscribeDecodeParams(workDir, tag string) error {
	dir := filepath.Join(workDir, repair.RetranscribeDir)
	markerPath := filepath.Join(dir, retranscribeDecodeMarker)
	if cur, err := os.ReadFile(markerPath); err == nil && strings.TrimSpace(string(cur)) == tag { //nolint:gosec // path derives from the book's own work dir
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, ent.Name())); err != nil {
			return err
		}
	}
	return fsutil.WriteFileAtomic(markerPath, []byte(tag+"\n"), 0o644)
}

// retranscribeChapter re-transcribes one chapter FRESH into retranscribe/, then uses
// repair.AdoptFresh to decide (never blindly) whether the fresh run replaces the
// original. On adopt it replaces the immutable raw (unfreeze -> write -> re-freeze
// 0444), re-derives the json/text layers, and drops the chapter's stale
// repaired/corrected files (correcting re-runs fully). On keep it leaves everything.
// It returns whether the fresh run was adopted.
func (e *Executor) retranscribeChapter(ctx context.Context, setup ASRSetup, book store.Book, durationSec float64, chapter int) (bool, error) {
	flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(chapter))
	if !fsutil.IsFile(flac) {
		return false, fmt.Errorf("retranscribing: chapter %d FLAC missing (%s); split must run first", chapter, flac)
	}
	freshDir := filepath.Join(book.WorkDir, repair.RetranscribeDir)
	if err := os.MkdirAll(freshDir, 0o750); err != nil {
		return false, err
	}
	freshRawPath := filepath.Join(freshDir, transcript.RawName(chapter))
	// Resume-idempotent: reuse a fresh raw a prior (interrupted) run already produced
	// rather than re-running the expensive full-chapter ASR. The completeness test is
	// the same one asrStage uses (transcript.Complete via rawComplete). The adopt
	// decision below is itself idempotent - after an ADOPT the fresh and main raw hold
	// identical content (AdoptFresh then keeps, a no-op re-derive), after a KEEP nothing
	// changed - so re-deciding a reused raw never corrupts a chapter.
	if !rawComplete(freshRawPath) {
		// Prompt-free (no seeded initial prompt: a guess makes a wrong spelling recur),
		// but NoContext-on: unlike asrStage, a repair re-transcription must vary the
		// decode params - context-conditioning drives the deterministic repetition
		// collapse we are here to fix, so an identical-params retry would just replay it.
		job := asr.Job{Audio: flac, OutDir: freshDir, Chapter: chapter, Language: setup.Language, NoContext: true}
		if terr := setup.Backend.Transcribe(ctx, job); terr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, fmt.Errorf("retranscribing: transcribe chapter %d: %w", chapter, terr)
		}
	}
	freshRaw, err := os.ReadFile(freshRawPath) //nolint:gosec // path derives from the book's work dir
	if err != nil {
		return false, fmt.Errorf("retranscribing: read fresh chapter %d: %w", chapter, err)
	}
	meta := transcript.Meta{Chapter: chapter, Backend: setup.Backend.ID(), Model: setup.Model, Language: setup.Language}
	freshT, err := transcript.Normalize(freshRaw, meta)
	if err != nil {
		return false, fmt.Errorf("retranscribing: normalize fresh chapter %d: %w", chapter, err)
	}

	origWords := 0
	if origT, oerr := transcript.ReadNormalized(filepath.Join(book.WorkDir, transcript.JSONDir), chapter); oerr == nil {
		origWords = len(strings.Fields(transcript.PlainText(origT)))
	}
	freshWords := len(strings.Fields(transcript.PlainText(freshT)))
	decision := repair.AdoptFresh(
		repair.AdoptStats{Words: origWords, DurationSec: durationSec},
		repair.AdoptStats{Words: freshWords, DurationSec: durationSec},
	)
	if !decision.Adopt {
		return false, nil
	}

	// Adopt: replace the immutable raw, re-derive the json/text layers, drop stale
	// repaired/corrected for this chapter.
	rawPath := filepath.Join(book.WorkDir, transcript.RawDir, transcript.RawName(chapter))
	_ = os.Chmod(rawPath, 0o644) //nolint:gosec // lift the immutability guard to replace a re-transcribed chapter
	if err := fsutil.WriteFileAtomic(rawPath, freshRaw, 0o644); err != nil {
		return false, fmt.Errorf("retranscribing: replace raw chapter %d: %w", chapter, err)
	}
	if err := os.Chmod(rawPath, 0o444); err != nil { //nolint:gosec // re-freeze the raw layer
		e.log.Warn("retranscribing: could not re-freeze raw", "path", rawPath, "err", err)
	}
	// Re-derive the json/text layers through the shared helper (from freshRaw, identical
	// to freshT) so this path cannot drift from sanitize's derivation.
	if err := deriveChapterLayers(book.WorkDir, freshRaw, meta); err != nil {
		return false, fmt.Errorf("retranscribing: %w", err)
	}
	removeChapterDerived(book.WorkDir, chapter)
	return true, nil
}

// clipChapter runs one chapter's mechanical clip repair - the shared spine of the tail
// (ClipAndSplice) and mid (ClipAndSpliceWindow) paths, which differ only in the request's
// window-override fields and the repair func passed as splice. It is resume-idempotent (an
// already-repaired chapter - repaired file + verdict, via the pass's ledger snapshot - is
// skipped whole; re-running would re-cut/re-transcribe/re-splice and append a DUPLICATE
// repairs.log line, so a re-adjudication that wants a chapter redone must express it as
// "retranscribe", never another clip). It reads the chapter's normalized transcript, parks
// when no clip cutter is available, re-transcribes the window prompt-free + NoContext (a
// seeded prompt makes the model echo it over sparse audio; the loop being cut is a
// context-conditioned collapse, so re-transcribing without context is what lets it resolve
// differently instead of replaying), calls splice, and drops the chapter's stale corrected
// file on a splice (correcting re-runs fully). label ("tail-clip" / "mid-clip") shapes the
// wrapped error text. It returns the full repair.ClipResult so the caller can distinguish a
// splice, a re-degeneration, a known-failed skip, and the tail unlocatable no-op (which it
// buckets as clips_unlocatable and re-queues asking for a clip_start_sec).
func (e *Executor) clipChapter(ctx context.Context, setup ASRSetup, cut repair.ClipCutter, book store.Book, chapter int, verdicts map[int]repair.TailVerdict, label string, req repair.ClipSpliceRequest, splice func(context.Context, repair.ClipSpliceRequest) (repair.ClipResult, error)) (repair.ClipResult, error) {
	if tailClipAlreadyDone(book.WorkDir, chapter, verdicts) {
		return repair.ClipResult{Chapter: chapter, Spliced: true}, nil // a prior run already spliced this chapter
	}
	origT, err := transcript.ReadNormalized(filepath.Join(book.WorkDir, transcript.JSONDir), chapter)
	if err != nil {
		return repair.ClipResult{Chapter: chapter}, fmt.Errorf("retranscribing: read chapter %d transcript: %w", chapter, err)
	}
	if cut == nil {
		return repair.ClipResult{Chapter: chapter}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	transcribe := func(ctx context.Context, clipPath string) ([]byte, error) {
		outDir := filepath.Join(book.WorkDir, repair.ClipsDir)
		// The backend names the raw from the audio stem (asr.RawOutputName), so the read
		// derives from clipPath (t/m NNN...flac), not chNNN.
		job := asr.Job{Audio: clipPath, OutDir: outDir, Chapter: chapter, Language: setup.Language, NoContext: true}
		if terr := setup.Backend.Transcribe(ctx, job); terr != nil {
			return nil, terr
		}
		return os.ReadFile(filepath.Join(outDir, asr.RawOutputName(clipPath))) //nolint:gosec // path derives from the book's work dir
	}
	req.WorkDir = book.WorkDir
	req.Chapter = chapter
	req.Transcript = origT
	req.Cut = cut
	req.Transcribe = transcribe
	req.DecodeTag = retranscribeDecodeTag
	res, err := splice(ctx, req)
	if err != nil {
		return repair.ClipResult{Chapter: chapter}, fmt.Errorf("retranscribing: %s chapter %d: %w", label, chapter, err)
	}
	if res.Spliced {
		_ = os.Remove(filepath.Join(book.WorkDir, spelling.CorrectedDir, transcript.TextName(chapter)))
	}
	return res, nil
}

// tailClipChapter is the thin tail-repair wrapper over clipChapter: locate the tail loop,
// cut+re-transcribe the window prompt-free, adjudicate, splice unless the clip re-
// degenerated. startOverrideSec, when > 0, is the agent-supplied window start (from the
// plan entry's clip_start_sec) that relocates a window whose derived cut kept re-
// degenerating; 0 derives as usual.
func (e *Executor) tailClipChapter(ctx context.Context, setup ASRSetup, cut repair.ClipCutter, book store.Book, chapterEnd float64, chapter int, startOverrideSec float64, verdicts map[int]repair.TailVerdict) (repair.ClipResult, error) {
	return e.clipChapter(ctx, setup, cut, book, chapter, verdicts, "tail-clip",
		repair.ClipSpliceRequest{ChapterEnd: chapterEnd, StartOverrideSec: startOverrideSec},
		repair.ClipAndSplice)
}

// midClipChapter is the thin MID-CHAPTER interior-repair wrapper over clipChapter: snap the
// agent's [startSec, endSec] window to segment edges, cut+re-transcribe it prompt-free,
// health-check, splice between the intact head and tail unless the clip re-degenerated. The
// window comes from the plan entry's clip_start_sec/clip_end_sec (Validate guarantees
// start>0 and end>start). chapterEnd rides on the request for parity with the tail path.
func (e *Executor) midClipChapter(ctx context.Context, setup ASRSetup, cut repair.ClipCutter, book store.Book, chapterEnd float64, chapter int, startSec, endSec float64, verdicts map[int]repair.TailVerdict) (repair.ClipResult, error) {
	// Clamp an over-long agent window to the chapter end: an endSec past EOF would leave an
	// empty tail (no segment starts after it) and silently turn the interior splice into a
	// tail-to-EOF one, discarding real narration the agent meant to keep. ChapterEnd > 0
	// here (from the manifest); a degenerate clamped window (end <= start) surfaces as the
	// ClipAndSpliceWindow validation error rather than a bad splice.
	if chapterEnd > 0 && endSec > chapterEnd {
		endSec = chapterEnd
	}
	return e.clipChapter(ctx, setup, cut, book, chapter, verdicts, "mid-clip",
		repair.ClipSpliceRequest{ChapterEnd: chapterEnd, StartOverrideSec: startSec, EndOverrideSec: endSec},
		repair.ClipAndSpliceWindow)
}

// tailClipAlreadyDone reports whether a prior clip run already spliced this chapter: both
// transcripts-repaired/<ch>.txt (the splice) and a tail_verdicts.json entry for the chapter
// (the adjudication record) are present. That pair is the durable evidence a successful
// tail OR mid splice writes, so its presence makes re-running the entry a no-op (and
// prevents a duplicate repairs.log line on resume). A CLIP-REDEGENERATED chapter writes
// only the verdict, not a repaired file, so it is NOT skipped - a resume legitimately
// re-attempts it. verdicts is the pass's one-shot ledger snapshot (see retranscribe), so
// this is a pure file check plus a map lookup, no per-call disk read.
func tailClipAlreadyDone(workDir string, chapter int, verdicts map[int]repair.TailVerdict) bool {
	if !fsutil.IsFile(filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(chapter))) {
		return false
	}
	_, ok := verdicts[chapter]
	return ok
}

// removeChapterDerived drops a chapter's stale repaired and corrected text so a later
// correcting run re-derives them from the adopted raw (both are idempotent re-derives,
// so removing them is always safe).
func removeChapterDerived(workDir string, chapter int) {
	_ = os.Remove(filepath.Join(workDir, transcript.RepairedDir, transcript.TextName(chapter)))
	_ = os.Remove(filepath.Join(workDir, spelling.CorrectedDir, transcript.TextName(chapter)))
}
