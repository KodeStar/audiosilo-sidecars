# Agent capacity and per-book fan-out

AudioSilo separates breadth across the queue from parallel work inside one book:

```yaml
agent:
  queue_concurrency: 2
  max_agents_per_book: 3
```

`queue_concurrency` is the number of books allowed to occupy agent stages. The
existing series lock still applies: only the earliest unfinished book in a series
may enter agent work. `max_agents_per_book` is the number of independent agent
invocations a supported stage may run for that eligible book. New configurations
use their product as the hard daemon-wide invocation bound, so the example permits
at most two agent-stage books and six simultaneous backend processes.

Both dimensions must be between 1 and 32 and their product must not exceed 64.
ASR remains a separate serial lane, and the mechanical lane remains independent.
All capacity changes are restart-to-apply.

## Legacy configuration migration

`agent.concurrency` is deprecated but remains supported. A file containing only:

```yaml
agent:
  concurrency: 4
```

retains the historical behavior: four books may occupy the agent lane and the
global backend-process cap remains four. It is deliberately not interpreted as
four books times four invocations. Loading this file does not rewrite it. The
settings API reports the normalized dimensions, effective global limit, and a
`legacy_concurrency` flag. Saving the two new settings explicitly migrates the
configuration; the old `concurrency` field is then omitted.

The environment equivalents are
`AUDIOSILO_SIDECARS_AGENT_QUEUE_CONCURRENCY` and
`AUDIOSILO_SIDECARS_AGENT_MAX_AGENTS_PER_BOOK`. The old
`AUDIOSILO_SIDECARS_AGENT_CONCURRENCY` retains the same legacy behavior.

## Stages that fan out

Only stages with a bounded fragment-and-merge contract fan out:

- `fact_pass` runs `min(max_agents_per_book, incomplete chunks)` extraction
  workers. Each worker receives only its chunk, writes its existing resumable
  chunk artifact in an isolated staging directory, and shares the global
  invocation semaphore. The final knowledge-sheet assembly remains serial and
  deterministic. The first error cancels the sibling workers and all workers are
  drained before the stage returns.
- `qa_adjudicating` partitions independent, non-auto-accepted chapters using a
  deterministic round-robin assignment. Each worker receives only its findings,
  manifest chapters, transcripts, and applicable prior outcomes; it writes an
  isolated plan fragment. Fragments and deterministic auto-accept entries are
  sorted by chapter and passed through the existing whole-plan validator before
  the plan is persisted. Repair-loop and non-convergence rules are unchanged.

`max_agents_per_book: 1` preserves serial execution for both stages. Partial fact
artifacts remain resumable after cancellation or failure.

## Intentionally serial stages

Synthesis remains serial. Characters and recaps consume the same whole-book fact
sheet and series carry-over, and the current validator checks their cross-file
knowledge as one authored result. Independent calls would duplicate the large
input, risk divergent names/facts, and weaken the cross-file consistency contract;
there is no safe deterministic merge that offsets that cost.

Spelling research, whole-book auditing, and fixing also remain serial because they
enforce global terminology or cross-file consistency. ASR and QA retranscription
remain capacity one for the current Metal backend.

Mechanical stages do not consume agent slots and are unchanged. Splitting is
ffmpeg/resource dominated and has no measured safe dedicated worker limit yet.
Sanitizing and correcting are cheap deterministic whole-book passes, so chapter
fan-out would add coordination and write-order complexity without a meaningful
expected improvement. These choices can be revisited with measurements and a
separate mechanical resource limit.

## Persistence, liveness, and cost

Migration `0010_agent_invocations.sql` adds one durable row per concrete backend
attempt. A row records its parent stage run, book/stage/bounded work unit,
backend/model, PID and active state, heartbeat/progress timestamps, start and
completion, outcome, tokens, provider-reported cost, and versioned API-equivalent
estimate. Validation failures, backend failures, cancellations, and retries are
separate attempts.

Invocation rows are the accounting source of truth. The parent `stage_runs` usage
and cost columns are a transactionally rebuilt compatibility summary, never an
independent increment. Concurrent completions therefore cannot lose updates or
double count. Failed, cancelled, retried, and invocations belonging to superseded
stage runs remain part of actual expenditure.

Supervisor liveness checks every active child PID for an open stage run. A missing
child is not hidden by a surviving sibling. Pre-migration open runs retain the
legacy parent-PID fallback. Cancellation, pause, shutdown, and supervisor
termination cancel the parent context; fanned-out workers drain, release both
per-book and global slots, and persist their final child state.

Queue statistics, supervisor context, the settings API, and the Running UI expose
agent-book occupancy, global invocation occupancy, per-book invocation counts,
fan-out support, and current/completed/remaining work units. Inefficient-slot
detection distinguishes a queued book slot from queued work behind an idle
per-book invocation slot.

## ETA and elapsed-time definitions

Agent rates are learned as topology-neutral seconds per completed invocation unit.
Fact assembly is kept as a bounded serial component. Supported-stage ETA uses
`ceil(remaining units / per-book fan-out)` waves. Queue ETA additionally clamps
fan-out to the effective global pool divided over admitted agent books; this is
especially important for legacy configurations whose global cap is not a product.

The compatible `started_at` remains the first stage-run start and is labeled batch
elapsed. First-class timing fields are:

- **Pre-ASR wall:** first stage start through the latest non-superseded successful
  primary `asr` completion.
- **ASR active:** the sum of actual primary `asr` attempt intervals, including
  failed and superseded attempts. Later `retranscribing` work is not primary ASR
  and does not reset the baseline.
- **Post-ASR elapsed:** that stable primary-ASR completion through now, or through
  the final stage completion for a done book. This is the prominent comparison.
- **Active processing:** the sum of actual stage-run intervals, including retries
  and superseded work. Per-book stages are serial, so these intervals do not
  overlap under the scheduler contract.
- **Queue/wait:** non-processing wall time after the first stage began, calculated
  as batch elapsed minus active processing. It can include deliberate pause or
  parked time; the UI labels it as wait/non-processing rather than claiming all of
  it was scheduler contention.

Legacy databases are backfilled conservatively from existing `stage_runs`; no
historical timestamp is invented.

## Testing without production data

The automated tests create temporary SQLite databases and work directories. Never
start a development daemon with the production data directory. If real artifacts
are needed after production has stopped, copy them first and run only against the
copy:

```sh
test_data="$(mktemp -d)"
cp -p /path/to/source/sidecars.db "$test_data/sidecars.db"
cp -R /path/to/source/work "$test_data/work"
go run ./cmd/audiosilo-sidecars serve --data "$test_data" --listen 127.0.0.1:18090
```

Useful stopped/copied-database inspection:

```sh
sqlite3 sidecars-copy.db 'SELECT book_id,stage,work_unit,status,process_id,input_tokens,output_tokens,cost_usd,started_at,completed_at FROM agent_invocations ORDER BY id DESC LIMIT 50;'
```
