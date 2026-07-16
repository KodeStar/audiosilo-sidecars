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
