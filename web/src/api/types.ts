// Wire types mirrored by hand from the Go daemon's /api/v1 contract.
// Keep these in sync with the daemon; there is no codegen.

export interface LoginResponse {
  token: string;
}

export interface ErrorResponse {
  error: string;
}

export type TabStatus = 'planned' | 'ready';

export interface SystemTab {
  id: string;
  label: string;
  status: TabStatus;
}

// Resolved media-tool paths (empty when a tool could not be located). Read-only
// diagnostic info surfaced on /system.
export interface ToolsInfo {
  ffmpeg: string;
  ffprobe: string;
}

// Resolved speech-recognition backend capability (mirrors the Go /system `asr`
// block). Read-only diagnostic info: whether ASR will run and on what device.
export interface AsrInfo {
  backend: string;
  available: boolean;
  device: string;
  version: string;
  detail: string;
}

// Resolved agent-runner capability (mirrors the Go /system `agent` block): which
// backend (claude/codex/"") will run the agent stages and whether it is usable.
// version/detail are omitempty on the wire, so optional here.
export interface AgentInfo {
  backend: string;
  available: boolean;
  version?: string;
  detail?: string;
}

export interface SupervisorRuntime {
  active_books: Record<string, boolean>;
  agent_active: number;
  agent_capacity: number;
  eligible_agent_books: number;
  eligible_agent_book_ids?: number[];
  agent_invocations?: number;
  invocation_capacity?: number;
  invocations_by_book?: Record<string, number>;
  max_agents_per_book?: number;
}

export interface SupervisorStatus {
  state: string;
  enabled: boolean;
  automatic_actions: boolean;
  model_assisted: boolean;
  model_available: boolean;
  allow_backend_failover: boolean;
  last_check_at?: string;
  last_error?: string;
  runtime: SupervisorRuntime;
}

export interface SystemInfo {
  version: string;
  data_dir: string;
  listen: string;
  tabs: SystemTab[];
  tools: ToolsInfo;
  asr: AsrInfo;
  agent: AgentInfo;
  supervisor?: SupervisorStatus;
  // Daemon-total on-disk scratch (sum of every book's work dir), the disk gauge.
  scratch_bytes: number;
}

// Secrets are presence booleans only - the actual values never cross the wire.
export interface SecretsPresence {
  anthropic_api_key: boolean;
  openai_api_key: boolean;
  github_pat: boolean;
}

export interface AsrConfig {
  backend: string;
}

// AgentConfig mirrors the Go settings `agent` view: the backend selector, the
// scheduler concurrency and per-invocation timeout, and the per-stage model maps
// (agent-stage name -> model; an absent/empty entry means the backend default).
export interface AgentConfig {
  backend: string;
  concurrency: number;
  queue_concurrency?: number;
  max_agents_per_book?: number;
  effective_global_invocation_limit?: number;
  legacy_concurrency?: boolean;
  timeout_minutes: number;
  book_budget_usd: number;
  claude_models: Record<string, string>;
  openai_models: Record<string, string>;
}

// ContributionConfig mirrors the Go settings `contribution` view (M7): how the
// contributing stage publishes a book's sidecars and how the intake poller runs.
// mode is issue | pr | local; repo is owner/name; auto_purge reclaims scratch when
// a book reaches done; poll_minutes is the open-contribution poll interval.
export interface ContributionConfig {
  mode: string;
  repo: string;
  auto_purge: boolean;
  poll_minutes: number;
}

export interface SupervisorConfig {
  enabled: boolean;
  automatic_actions: boolean;
  model_assisted: boolean;
  model_automatic_actions: boolean;
  allow_backend_failover: boolean;
}

export interface Settings {
  listen: string;
  cors_origins: string[];
  secrets: SecretsPresence;
  asr: AsrConfig;
  agent: AgentConfig;
  contribution: ContributionConfig;
  supervisor: SupervisorConfig;
}

// ContributionUpdate is the optional contribution envelope of PUT /settings. Each
// field is left untouched when omitted. Like the agent section, changes are
// persisted but only take effect on a daemon RESTART.
export interface ContributionUpdate {
  mode?: string;
  repo?: string;
  auto_purge?: boolean;
  poll_minutes?: number;
}

// AgentUpdate is the optional agent envelope of PUT /settings. Scalar fields are
// left untouched when omitted; the model maps replace the corresponding config map
// wholesale when present (so an omitted stage = the backend default). Agent changes
// are persisted but only take effect on a daemon RESTART.
export interface AgentUpdate {
  backend?: string;
  concurrency?: number;
  queue_concurrency?: number;
  max_agents_per_book?: number;
  timeout_minutes?: number;
  book_budget_usd?: number;
  claude_models?: Record<string, string>;
  openai_models?: Record<string, string>;
}

export type SupervisorUpdate = Partial<SupervisorConfig>;

// Keys understood by PUT /settings. A non-empty secret string sets it, an empty
// string clears it, an omitted key is left untouched.
export interface SettingsUpdate {
  cors_origins?: string[];
  secrets?: Partial<Record<keyof SecretsPresence, string>>;
  agent?: AgentUpdate;
  contribution?: ContributionUpdate;
  supervisor?: SupervisorUpdate;
}

export interface ChangePasswordBody {
  current: string;
  new: string;
}

// --- pipeline: scans (mirrors internal/metaops + internal/api handlers_pipeline) ---

export type ScanStatus = 'running' | 'done' | 'error';

// How a book's identity resolved to a known work. asin/isbn are automatic exact
// matches; search is an automatic title-search match; manual is a user override.
// Absent means no match (unknown work).
export type MatchedBy = 'asin' | 'isbn' | 'search' | 'manual';

export interface ScanProgress {
  phase: string; // "scanning" | "coverage" | "done"
  // Directory-walk progress, reported while the tree is enumerated (before
  // groups_total is known). Shown during the scanning phase until groups_total > 0.
  walk_dirs: number;
  walk_groups: number;
  groups_done: number;
  groups_total: number;
  books_found: number;
  coverage_done: number;
  coverage_total: number;
}

// Coverage is the per-book metadata verdict. Known/HasCharacters/HasRecaps are
// meaningful only when available === true. matched_by/work_title describe how the
// identity resolved (set for search/manual matches, so the UI can show provenance).
export interface Coverage {
  available: boolean;
  known: boolean;
  work_id?: string;
  has_characters: boolean;
  has_recaps: boolean;
  matched_by?: MatchedBy;
  work_title?: string;
  // Primary series membership supplied by the matched metadata work. A known
  // match takes precedence over absent/weak local scan metadata at enqueue time.
  series?: MetaSearchSeries | null;
}

export interface ScannedBook {
  // Root-relative folder (display + in-scan selection key).
  path: string;
  // ABSOLUTE folder - the daemon-computed durable identity every API call keys
  // on (candidates, overrides). Never derive it by joining paths client-side.
  source_path: string;
  title: string;
  subtitle?: string;
  authors?: string[];
  narrators?: string[];
  series?: string;
  series_position?: string;
  asin?: string;
  isbn?: string;
  runtime_min?: number;
  chapters?: number;
  audio_files: number;
  // Where each field came from ("tag" | "path" | "filename" | "metadata").
  sources?: Record<string, string>;
  coverage: Coverage;
  // True when the user has hidden this book from the default candidate list (a
  // persisted daemon-side override). Excluded from the default view; re-showable.
  hidden?: boolean;
}

// ScanStats is the end-of-scan summary the daemon attaches once status is "done".
// The exact keys are daemon-owned; we do not render them field-by-field yet.
export type ScanStats = Record<string, number>;

// ScanJob is the full poll view. books is TOP-LEVEL and grows incrementally while
// running; identity/coverage fields are provisional until status === "done", when
// the authoritative list replaces the array (re-render from books, keyed by path).
export interface ScanJob {
  id: string;
  path: string;
  status: ScanStatus;
  error?: string;
  started_at?: string;
  progress: ScanProgress;
  books: ScannedBook[];
  stats?: ScanStats;
}

// ScanSummary is a ScanJob without its books - the shape GET /scans returns per
// job, used to reattach to an in-flight/last scan after a page reload.
export type ScanSummary = Omit<ScanJob, 'books'>;

export interface ListScansResponse {
  scans: ScanSummary[];
}

export interface CreateScanResponse {
  job_id: string;
}

// --- pipeline: overrides (persisted per-book hide + manual-match state) ---

// Override is the daemon-persisted per-book override: hidden and/or a manual
// work_id match. work_title is the display label for a manual match. work_id/
// work_title/updated_at are optional to mirror the Go DTO's omitempty (a hide-only
// override omits them).
export interface Override {
  source_path: string;
  hidden: boolean;
  work_id?: string;
  work_title?: string;
  updated_at?: string;
}

// SetOverrideBody is a FULL desired-state upsert (hidden=false + work_id="" clears
// the override). POST /overrides.
export interface SetOverrideBody {
  source_path: string;
  hidden: boolean;
  work_id: string;
}

// SetOverrideResponse echoes the stored override plus the recomputed coverage when
// a work_id was set (matched_by "manual"); coverage is null otherwise.
export interface SetOverrideResponse {
  override: Override;
  coverage: Coverage | null;
}

// --- meta search (manual-match lookup against meta.audiosilo.app) ---

export interface MetaSearchSeries {
  name: string;
  position: string;
}

export interface MetaSearchResult {
  id: string;
  title: string;
  authors: string[];
  series: MetaSearchSeries | null;
  cover_url: string;
}

export interface MetaSearchResponse {
  results: MetaSearchResult[];
}

// --- pipeline: books ---

// BookCandidate is one selected book to enqueue (POST /books body item).
// coverage + sources are the advisory scan-time snapshot the daemon persists and
// echoes back on the book view.
export interface BookCandidate {
  source_path: string;
  title: string;
  authors: string[];
  // Narrator credits from the scan, carried through so a later core proposal can
  // prefill them (metaissue requires >= 1 narrator). Omitted when the scan found none.
  narrators?: string[];
  series: string;
  series_pos: string;
  asin: string;
  isbn: string;
  coverage?: Coverage;
  sources?: Record<string, string>;
  // A manual-match work id carried from an override, so the enqueued book keeps
  // the identity the user picked. Omitted when the book has no manual match.
  work_id?: string;
}

export interface CreateBooksRequest {
  candidates: BookCandidate[];
}

export interface BookProgress {
  stage: string;
  done: number;
  total: number;
}

// The three sidecar dimensions a book can contribute. core is the metadata work
// proposal (add-work), needed when the book's work does not yet exist upstream.
export type ContributionKind = 'characters' | 'recaps' | 'core';

// How a single contribution was published (the config mode at submit time).
export type ContributionMode = 'issue' | 'pr' | 'local';

// A single contribution's lifecycle status. submitted/pr_open are open; merged/
// closed/local/already_covered are terminal. The aggregate summary on BookView
// collapses the per-kind rows to one of submitted | pr_open | merged | closed |
// local (see the daemon's aggregate helper).
export type ContributionStatus =
  'submitted' | 'pr_open' | 'merged' | 'closed' | 'local' | 'already_covered';

// ContributionSummary is the aggregate chip shown on a done book (BookView). It is
// ABSENT when the book has no contribution rows (a legacy/local book), which the UI
// renders as the "Local only" chip. url is the best link (issue or PR), '' when none.
export interface ContributionSummary {
  status: string;
  url: string;
}

// ContributionRow is one per-kind contribution record from the book detail view.
// number is the issue number (issue mode) or PR number (pr mode); pr_number/pr_url
// point at the intake bot's PR once it opens (issue mode). note carries any caveat
// (e.g. "labels missing - a maintainer must apply data:characters").
export interface ContributionRow {
  kind: ContributionKind;
  mode: ContributionMode;
  repo: string;
  number: number;
  url: string;
  pr_number: number;
  pr_url: string;
  status: string;
  note: string;
  created_at: string;
  updated_at: string;
}

export interface BookView {
  id: number;
  batch_id?: string;
  source_path: string;
  title: string;
  authors: string[];
  series?: string;
  series_pos?: string;
  asin?: string;
  isbn?: string;
  // The matched meta work slug, once resolved (manual match at enqueue, an
  // asin/isbn/search match, or a core proposal that merged). Empty/absent until then.
  work_id?: string;
  state: string;
  // lane is the served lane the current state runs in ('asr' | 'agent' |
  // 'mechanical' | '' for a waypoint). The daemon computes it (state.LaneOf), so
  // the web UI no longer mirrors the state->lane table.
  lane: string;
  status: string;
  error?: string;
  // Typed machine-readable park reason beside the free-text error (see the
  // internal/state ParkCode enum). Present (non-empty) only while parked
  // (status === 'needs_attention'); cleared on retry/resume/cancel. omitempty.
  park_code?: string;
  // Scheduled automatic re-admit instant (RFC3339 UTC) for a book parked on a transient
  // agent condition (agent_unavailable/agent_rate_limited); absent for a plain park or a
  // book predating the feature. Its presence flips the park hint to "retries
  // automatically". Carried on the initial GET /books fetch AND on the book.state SSE
  // patch (applyBookState mirrors it, clearing it when a frame carries none).
  retry_at?: string;
  coverage?: Coverage;
  identity_sources?: Record<string, string>;
  progress: BookProgress[];
  // Estimated seconds until the book reaches ready/done, from the scheduler's
  // latest ETA snapshot. Present only for an active, unparked book. omitempty.
  eta_seconds?: number;
  // When the first stage run for the book began (RFC3339); MIN(stage_runs.started_at).
  // Absent until the book has started running. omitempty.
  started_at?: string;
  timing?: BookTiming;
  active_agent_invocations?: number;
  max_agents_per_book?: number;
  fanout_supported?: boolean;
  current_work_units?: number;
  completed_work_units?: number;
  remaining_work_units?: number;
  // Current on-disk size of the book's work dir in bytes (chapters + durables);
  // 0 when not yet created or already purged.
  scratch_bytes: number;
  // Total audio duration in seconds, written after inspect. Absent (or 0) before
  // inspect / for a pre-migration book - the Running list hides the length chip.
  duration_sec?: number;
  // Summed agent spend across the book's stage runs in USD (0 for a book that has
  // run only mechanical/ASR stages or none yet, or when the backend reports no cost).
  // Present on both the list and detail views.
  total_cost_usd: number;
  // Aggregate contribution chip (M7): the one status shown on the Done board.
  // ABSENT when the book has no contribution rows - the UI shows "Local only".
  contribution?: ContributionSummary;
  created_at: string;
  updated_at: string;
}

export interface BookTiming {
  batch_started_at?: string;
  primary_asr_completed_at?: string;
  batch_elapsed_seconds?: number;
  pre_asr_wall_seconds?: number;
  asr_active_seconds?: number;
  post_asr_elapsed_seconds?: number;
  active_processing_seconds?: number;
  queue_wait_seconds?: number;
}

export interface AgentInvocation {
  id: number;
  stage_run_id: number;
  book_id: number;
  stage: string;
  work_unit: string;
  backend: string;
  model: string;
  process_id?: number;
  active: boolean;
  heartbeat_at: string;
  progress_at: string;
  started_at: string;
  completed_at?: string;
  status: 'running' | 'success' | 'validation_failed' | 'failure' | 'cancelled';
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cost_usd: number;
  cost_reported: boolean;
  estimated_api_cost_usd?: number;
  estimate_complete: boolean;
  error?: string;
}

// StageRun is one execution (or attempt) of a stage for a book, from the book
// detail ledger. Model/InputTokens/OutputTokens/CostUSD (M5) capture agent spend;
// mechanical/ASR stages leave them zero. cost_usd is 0 when the backend reports no
// USD cost (codex). ok is null while running, true on success, false on failure.
export interface StageRun {
  id: number;
  book_id: number;
  stage: string;
  attempt: number;
  started_at: string;
  finished_at: string;
  ok: boolean | null;
  metrics?: unknown;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens?: number;
  cost_usd: number;
  cost_reported?: boolean;
  estimated_api_cost_usd?: number;
  estimate_complete?: boolean;
  heartbeat_at?: string;
  progress_at?: string;
  process_id?: number;
  process_active?: boolean;
}

// BookDetail is GET /books/{id}: a BookView plus the per-execution stage-run ledger
// and the per-kind contribution rows (M7; absent/empty until the book contributes).
export interface BookDetail extends BookView {
  stage_runs: StageRun[];
  contributions?: ContributionRow[];
  agent_invocations?: AgentInvocation[];
}

// --- M7: core work proposal (add-work) ---

// A region-scoped Audible ASIN pair carried on a core proposal.
export interface RegionAsin {
  region: string;
  asin: string;
}

// CoreProposal is the add-work metadata proposal for a book whose work does not yet
// exist on AudioSilo Meta. It mirrors the Go contrib.CoreProposal / the GET and POST
// bodies of the contribute-core endpoints. The daemon requires title, >= 1 author,
// language, >= 1 narrator, and non-empty sources (see validateCoreForm, which mirrors
// it for immediate feedback). abridged is '' (unknown) | 'Unabridged' | 'Abridged'.
export interface CoreProposal {
  title: string;
  subtitle: string;
  authors: string[];
  language: string;
  first_published: string;
  series_name: string;
  series_position: string;
  print_isbns: string[];
  narrators: string[];
  abridged: '' | 'Unabridged' | 'Abridged';
  runtime_min: number;
  release_date: string;
  publisher: string;
  asins: RegionAsin[];
  audiobook_isbns: string[];
  cover_url: string;
  sources: string;
}

// SetBookWorkBody is the POST /books/{id}/work body: attach an existing meta work
// slug to a book (the "the work already exists" escape hatch).
export interface SetBookWorkBody {
  work_id: string;
}

export interface BookCreateResult {
  source_path: string;
  created: boolean;
  conflict?: boolean;
  error?: string;
  book?: BookView;
}

export interface CreateBooksResponse {
  batch_id: string;
  results: BookCreateResult[];
}

export interface SupervisorRun {
  id: number;
  incident_key?: string;
  batch_id: string;
  book_id?: number;
  stage_run_id?: number;
  trigger: string;
  diagnosis: string;
  confidence: number;
  evidence: string[];
  decision: string;
  selected_action: string;
  suggested_retry_limit: number;
  suggested_termination_limit: number;
  action_outcome: string;
  automatic: boolean;
  approval_required: boolean;
  state: string;
  model?: string;
  backend?: string;
  model_calls: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  provider_cost_usd?: number;
  provider_cost_complete: boolean;
  estimated_api_cost_usd?: number;
  estimate_complete: boolean;
  pricing_version?: string;
  started_at: string;
  completed_at?: string;
}

export interface BatchCostSummary {
  batch_id: string;
  production_reported_usd: number;
  production_reported_incomplete: boolean;
  production_estimated_api_usd: number;
  production_estimate_incomplete: boolean;
  book_supervisor_reported_usd: number;
  book_supervisor_estimated_api_usd: number;
  batch_supervisor_reported_usd: number;
  batch_supervisor_estimated_api_usd: number;
  supervisor_reported_incomplete: boolean;
  supervisor_estimate_incomplete: boolean;
  overall_reported_usd: number;
  overall_reported_incomplete: boolean;
  overall_estimated_api_usd: number;
  overall_estimate_incomplete: boolean;
}

export interface ListBooksResponse {
  books: BookView[];
}

// --- SSE event payloads (see internal/scheduler publish sites) ---

export interface BookStateEvent {
  book_id: number;
  state: string;
  lane: string;
  status: string;
  // The book's error string (a failed stage or cancel reason); '' when none.
  error: string;
  // The typed park reason (see the internal/state ParkCode enum); '' when none.
  park_code?: string;
  // The scheduled auto-readmit instant (RFC3339 UTC) the daemon set for this write, or
  // '' when it set none - so a park frame carries the retry_at that flips the hint to
  // "retries automatically" and an advance/clear frame carries '' to reset it.
  retry_at?: string;
}

export interface StageProgressEvent {
  book_id: number;
  stage: string;
  done: number;
  total: number;
}

// StageNoteEvent is the durable `stage.note` SSE frame: a human-readable liveness
// line (agent heartbeats, stage work-set descriptors) appended to the book's log.
// The line arrives via the REST log fetch; the frame only signals a refetch (see
// bumpBookEventCount in @/lib/books).
export interface StageNoteEvent {
  book_id: number;
  stage: string;
  msg: string;
}

export interface QueueStatsEvent {
  asr_active: number;
  agent_active: number;
  agent_books_active?: number;
  agent_book_capacity?: number;
  agent_invocations_active?: number;
  agent_invocation_capacity?: number;
  agent_invocations_by_book?: Record<string, number>;
  mechanical_active: number;
  queued: number;
}

// EtaUpdateEvent is the daemon-wide `eta.update` SSE frame (book_id 0). books
// lists only books that currently have an ETA (active, unparked). queue_seconds
// is the estimated makespan for the whole queue, or null when it cannot be
// estimated.
export interface EtaUpdateEvent {
  queue_seconds: number | null;
  books: { book_id: number; eta_seconds: number }[];
}

// ContributionUpdateEvent is the `contrib.update` SSE frame (M7): published by the
// contributing stage after a submission and by the intake poller on every status
// change. The Done tab refetches on it so the contribution chip stays live.
export interface ContributionUpdateEvent {
  book_id: number;
  kind: string;
  status: string;
  url: string;
}

// --- Done tab: sidecar preview (GET /books/{id}/sidecars) ---
// These mirror the metaserve-API-shaped preview the daemon serves so the vendored
// expressive.ts (src/lib/expressive.ts) consumes them unchanged. Field names/
// optionality track audiosilo-meta site/src/lib/api.ts.

// A spoiler position on a work's own (edition-independent) timeline. chapter is
// the logical book chapter; 0 = front matter / prior-book knowledge.
export interface Position {
  chapter: number;
}

// A community-authored, spoiler-tagged character entry (the CC BY-SA layer).
export interface Character {
  id: string;
  name: string;
  aliases?: string[];
  role?: 'protagonist' | 'antagonist' | 'supporting' | 'minor';
  reveal: Position;
  description?: string;
  xref?: { wikidata?: string; goodreads?: string };
}

// A position-keyed "story so far" recap. through = safe once that chapter is done.
export interface Recap {
  through: Position;
  scope?: 'book' | 'series';
  text: string;
}

// A work's whole-book recap summary (the CC BY-SA layer). in_short is the whole
// book in one paragraph, ending included; ending is a plain statement of how the
// book closes. Both optional and FULL spoilers - distinct from the position-gated
// chaptered recaps above.
export interface RecapSummary {
  in_short?: string;
  ending?: string;
}

// SidecarsView is GET /books/{id}/sidecars: the extracted characters/recaps
// preview for the Done tab. work is the matched work slug (or empty); the sidecar
// arrays/summary are omitempty on the wire.
export interface SidecarsView {
  work: string;
  characters?: Character[];
  recaps?: Recap[];
  recap_summary?: RecapSummary;
}

// --- Done tab: per-book event log (GET /books/{id}/events) ---

// LoggedEvent is one durable event-log row (newest first). payload is the event's
// JSON body, shape-varying by type, so it stays unknown.
export interface LoggedEvent {
  id: number;
  ts: string;
  type: string;
  payload?: unknown;
}

export interface BookEventsResponse {
  events: LoggedEvent[];
}
