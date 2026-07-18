# Bounded batch supervisor

The batch supervisor improves unattended multi-book runs without becoming a coding or
content agent. It is an orchestration service alongside the existing scheduler. It
reuses books, stage runs, sentinels, park codes, scheduler cancellation, retry, and
concurrency; it does not introduce a second job system.

## Architecture and safety boundary

There are two layers:

1. The deterministic monitor takes cheap snapshots of book state, stage-run history,
   heartbeat/progress timestamps, recorded process state, scheduler occupancy, errors,
   costs, and artifacts. It classifies known incidents and selects a fixed playbook.
2. The optional model supervisor is called only when a new event is ambiguous or
   unclassified, or when an operator clicks **Ask supervisor**. It is not called by
   every health tick. Incident keys suppress the same immutable event; a new stage-run
   attempt or changed fingerprint creates a new event.

Model context is deliberately bounded. It contains the book/stage state, at most eight
recent attempts, heartbeat and progress timestamps, usage/cost fields, compact
validation/QA/audit metrics, the error fingerprint and incident evidence, scheduler
occupancy, and at most eight small durable event tails. It contains no production
transcript. The runner uses a separate `<data>/supervisor` working directory, Claude's
enforced empty tool catalogue, and a strict JSON response schema. Codex read-only mode
still permits file inspection, so it fails closed as a supervisor backend rather than
relying on prompt instructions as a security boundary.

The model must return:

- `diagnosis`, `confidence`, and an `evidence` array;
- `recommended_action` from the compiled allowed-action enum;
- `human_approval_required`;
- non-negative suggested retry and termination limits.

Unknown fields, malformed JSON, invalid confidence, and actions outside the enum fail
closed. Neither deterministic nor model-assisted supervision has actions for editing
source, prompts, extracted facts, or generated documents; increasing budgets; making
an unapproved global backend change; or publishing/replacing GitHub outputs.

Supervision is not a production stage. `supervisor_runs` is a separate ledger, so it
cannot affect stage counts, convergence rounds, sentinels, or the production state
machine merely by recording a decision.

## Detection and recovery

The deterministic classifier covers:

- open stage runs missing a scheduler worker, or a recorded child PID which no longer
  exists;
- stale invocation heartbeats and lack of meaningful stage progress;
- configured duration, token, and reported-cost ceilings;
- duration, tokens, or cost growing beyond the configured factor versus the last
  successful attempt of the same stage;
- normalized repeated-error fingerprints (volatile paths, IDs, and counters removed);
- three identical QA metric fingerprints and flat/diverging audit fix counts;
- authentication, rate-limit/overload, and backend-unavailable signatures;
- absent, empty, or invalid completion sentinels and required artifacts;
- idle configured agent capacity while eligible books are waiting.

Available playbooks are `observe`, `retry`, `readmit`, `requeue`,
`terminate_requeue`, `supersede_rerun`, `stop_budget`, `reallocate`,
`fallback_backend`, `park_escalate`, and `ask_model`.

Safe playbooks only use existing scheduler/store mechanisms:

- transient rate-limit parks retain the scheduler's existing timed backoff and are
  readmitted when that window is due, within the configured attempt limit;
- an actual stuck worker is cancelled, or an orphaned open database run is durably
  failed, then the stage is requeued;
- an invalid non-publishing stage is superseded, its stage-and-later sentinels are
  removed, and the book rewinds using the same released code and inputs;
- a duration/token/cost runaway is parked with `supervisor_budget` before more work is
  admitted;
- available configured capacity is nudged through the normal scheduler;
- a fallback backend/model is activated only when all automatic/model-action gates are
  enabled and that exact fallback was configured in advance.

The contributing/publishing stage is never automatically superseded. Authentication,
repeated failures, non-converging repair loops, attempt-limit exhaustion, and any
unapproved backend change park with `supervisor_escalated` for operator review.
Automatic parking may contain a problem, but remediation/readmission remains a human
decision. Existing in-invocation transient retry logic remains the cheapest first
line of recovery.

## Configuration

All settings are restart-to-apply. Older configuration files inherit defaults because
loading starts from the current default envelope. The safe defaults enable only
deterministic observation: automatic actions, model calls, model actions, and failover
are all off.

```yaml
pricing:
  version: "operator-prices-2026-07"
  rates:
    "claude/*":
      input_usd_per_million: 5
      output_usd_per_million: 25
      cached_input_usd_per_million: 0.5
    "codex/gpt-example":
      input_usd_per_million: 2
      output_usd_per_million: 8
      cached_input_usd_per_million: 0.2

supervisor:
  enabled: true
  automatic_actions: false
  interval_seconds: 30
  stale_heartbeat_minutes: 20
  no_progress_minutes: 30
  max_stage_minutes: 180
  max_attempts: 3
  max_error_repeats: 2
  max_stage_tokens: 0
  max_stage_cost_usd: 0
  attempt_growth_factor: 3

  model_assisted: false
  model_automatic_actions: false
  model_backend: ""
  model: ""
  max_model_calls: 1
  max_turns: 8
  timeout_seconds: 90
  invocations_per_hour: 4
  per_book_budget_usd: 2
  overall_batch_budget_usd: 10

  allow_backend_failover: false
  fallback_backend: ""
  fallback_model: ""
```

`max_stage_tokens` and `max_stage_cost_usd` are disabled at zero; the wall-clock limit
and comparison factor remain active. The 20-minute stale default is deliberately
longer than the production runner's longest built-in 15-minute rate-limit backoff.
`model_backend` empty means the configured production runner when that runner can
enforce an empty tool catalogue. Currently Claude can; a Codex production runner makes
model supervision unavailable unless `model_backend: claude` is configured. An
explicit `model_backend: codex` is rejected by validation.
If model assistance is configured but no no-tools-capable backend is available,
ambiguous incidents retain the deterministic park/escalate path and the UI disables
manual model requests; deterministic monitoring never depends on model availability.
Pricing keys are exact `backend/model` pairs or an explicit `backend/*` fallback. Rates
are never fetched or silently updated by the daemon.

The five principal gates are also exposed in Settings. They are intentionally
separate: enabling model diagnosis does not enable model actions, and enabling model
actions does not approve backend failover. Configuration validation rejects model
automatic actions without model assistance and failover without a fallback backend.

Environment overrides are available for the three common operational switches:
`AUDIOSILO_SIDECARS_SUPERVISOR_ENABLED`,
`AUDIOSILO_SIDECARS_SUPERVISOR_AUTOMATIC_ACTIONS`, and
`AUDIOSILO_SIDECARS_SUPERVISOR_MODEL_ASSISTED`.

## Cost accounting

Migration `0009_batch_supervisor.sql` creates durable batch IDs and the separate
supervisor ledger. Every supervisor record stores its trigger, associations, diagnosis,
decision/action/outcome, automatic/approval flags, backend/model, actual model-call
count, input/output/cached tokens, nullable provider cost and its completeness flag,
nullable API-equivalent estimate and its completeness flag, pricing version, and
start/completion timestamps.

Provider-reported cost and API-equivalent estimated cost are different facts:

- `provider_cost_usd`/production `cost_usd` are actual values reported by the runner;
- `estimated_api_cost_usd` is calculated only from the explicitly versioned pricing
  table;
- unavailable cost is null/incomplete, not silently zero. In particular, a Codex production call
  with no provider cost and no matching configured price is not treated as free, and a
  model supervisor call is blocked if neither cost source is available.

The batch cost endpoint reports production work, book-attributed supervision,
batch-level supervision, and overall totals, with separate reported and API-equivalent
figures plus incomplete flags. Failed calls and superseded production attempts remain
included because expenditure is append-only. No hypothetical saving is subtracted
from actual cost. An avoided/recovered-cost estimate may be presented separately in a
future UI, but is not currently calculated.

## Inspecting and controlling supervision

The Running panel shows current state, automatic/model gates, split costs, and recent
decisions. Parked rows show typed escalation/budget hints. When model assistance is on,
an active book exposes **Ask supervisor**.

Authenticated API endpoints are:

```text
GET  /api/v1/supervisor/status
GET  /api/v1/supervisor/incidents?batch_id=<id>&limit=50
GET  /api/v1/supervisor/costs?batch_id=<id>
POST /api/v1/books/<book-id>/ask-supervisor
```

`GET /api/v1/system` also includes supervisor state. `GET`/`PUT
/api/v1/settings` expose the five principal gates. Decision events are published as
`supervisor.decision` over the existing authenticated SSE stream.

For direct local inspection of a stopped/copied database:

```sh
sqlite3 sidecars-copy.db \
  'SELECT id,batch_id,book_id,trigger,diagnosis,selected_action,state,action_outcome,started_at FROM supervisor_runs ORDER BY id DESC LIMIT 25;'
```

## Safe testing with copied data

Never point a development daemon at the live data directory. Stop the copied/test
daemon, make a private temporary directory, copy only what is needed, and pass that
copy with `--data`:

```sh
test_data="$(mktemp -d)"
cp -p /path/to/source/sidecars.db "$test_data/sidecars.db"
cp -R /path/to/source/work "$test_data/work"
go run ./cmd/audiosilo-sidecars serve --data "$test_data" --listen 127.0.0.1:18090
```

For classifier/recovery testing, prefer the in-memory/file-backed fixtures in
`internal/supervisor`, `internal/scheduler`, and `internal/store`. The simulated
multi-book test creates its own database and work directories and covers an orphaned
invocation recovery plus repeated-error escalation. With `supervisor.enabled: false`,
the service returns without inspecting, recording, or applying anything; existing
pipeline behavior is unchanged.

Run the focused tests with:

```sh
go test ./internal/pricing ./internal/store ./internal/supervisor ./internal/scheduler ./internal/api
cd web && npm test -- --run
```
