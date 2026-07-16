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

export interface SystemInfo {
  version: string;
  data_dir: string;
  listen: string;
  tabs: SystemTab[];
}

// Secrets are presence booleans only - the actual values never cross the wire.
export interface SecretsPresence {
  anthropic_api_key: boolean;
  openai_api_key: boolean;
  github_pat: boolean;
}

export interface AsrConfig {
  backend: string;
  device: string;
}

export interface AgentConfig {
  backend: string;
  concurrency: number;
}

export interface Settings {
  listen: string;
  cors_origins: string[];
  secrets: SecretsPresence;
  asr: AsrConfig;
  agent: AgentConfig;
}

// Keys understood by PUT /settings. A non-empty secret string sets it, an empty
// string clears it, an omitted key is left untouched.
export interface SettingsUpdate {
  cors_origins?: string[];
  secrets?: Partial<Record<keyof SecretsPresence, string>>;
}

export interface ChangePasswordBody {
  current: string;
  new: string;
}

// --- pipeline: scans (mirrors internal/metaops + internal/api handlers_pipeline) ---

export type ScanStatus = 'running' | 'done' | 'error';

export interface ScanProgress {
  phase: string; // "scanning" | "coverage" | "done"
  done: number;
  total: number;
}

// Coverage is the per-book metadata verdict. Known/HasCharacters/HasRecaps are
// meaningful only when available === true.
export interface Coverage {
  available: boolean;
  known: boolean;
  work_id?: string;
  has_characters: boolean;
  has_recaps: boolean;
}

export interface ScannedBook {
  path: string;
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
}

export interface ScanResult {
  root: string;
  books: ScannedBook[];
}

export interface ScanJob {
  id: string;
  path: string;
  status: ScanStatus;
  error?: string;
  progress: ScanProgress;
  result?: ScanResult;
}

export interface CreateScanResponse {
  job_id: string;
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
  coverage?: Coverage;
  identity_sources?: Record<string, string>;
  progress: BookProgress[];
  created_at: string;
  updated_at: string;
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
