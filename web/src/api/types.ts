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

export interface SystemInfo {
  version: string;
  data_dir: string;
  listen: string;
  tabs: SystemTab[];
  tools: ToolsInfo;
  asr: AsrInfo;
  agent: AgentInfo;
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
  timeout_minutes: number;
  claude_models: Record<string, string>;
  openai_models: Record<string, string>;
}

export interface Settings {
  listen: string;
  cors_origins: string[];
  secrets: SecretsPresence;
  asr: AsrConfig;
  agent: AgentConfig;
}

// AgentUpdate is the optional agent envelope of PUT /settings. Scalar fields are
// left untouched when omitted; the model maps replace the corresponding config map
// wholesale when present (so an omitted stage = the backend default). Agent changes
// are persisted but only take effect on a daemon RESTART.
export interface AgentUpdate {
  backend?: string;
  concurrency?: number;
  timeout_minutes?: number;
  claude_models?: Record<string, string>;
  openai_models?: Record<string, string>;
}

// Keys understood by PUT /settings. A non-empty secret string sets it, an empty
// string clears it, an omitted key is left untouched.
export interface SettingsUpdate {
  cors_origins?: string[];
  secrets?: Partial<Record<keyof SecretsPresence, string>>;
  agent?: AgentUpdate;
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
  // Where each field came from ("tag" | "path" | "filename").
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

export interface BookView {
  id: number;
  source_path: string;
  title: string;
  authors: string[];
  series?: string;
  series_pos?: string;
  asin?: string;
  isbn?: string;
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
  coverage?: Coverage;
  identity_sources?: Record<string, string>;
  progress: BookProgress[];
  // Estimated seconds until the book reaches ready/done, from the scheduler's
  // latest ETA snapshot. Present only for an active, unparked book. omitempty.
  eta_seconds?: number;
  // When the first stage run for the book began (RFC3339); MIN(stage_runs.started_at).
  // Absent until the book has started running. omitempty.
  started_at?: string;
  // Current on-disk size of the book's work dir in bytes (chapters + durables);
  // 0 when not yet created or already purged.
  scratch_bytes: number;
  // Summed agent spend across the book's stage runs in USD (0 for a book that has
  // run only mechanical/ASR stages or none yet, or when the backend reports no cost).
  // Present on both the list and detail views.
  total_cost_usd: number;
  created_at: string;
  updated_at: string;
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
  cost_usd: number;
}

// BookDetail is GET /books/{id}: a BookView plus the per-execution stage-run ledger.
export interface BookDetail extends BookView {
  stage_runs: StageRun[];
}

export interface BookCreateResult {
  source_path: string;
  created: boolean;
  conflict?: boolean;
  error?: string;
  book?: BookView;
}

export interface CreateBooksResponse {
  results: BookCreateResult[];
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
}

export interface StageProgressEvent {
  book_id: number;
  stage: string;
  done: number;
  total: number;
}

export interface QueueStatsEvent {
  asr_active: number;
  agent_active: number;
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
