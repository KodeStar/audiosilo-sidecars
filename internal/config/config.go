// Package config loads and validates the audiosilo-sidecars daemon configuration.
//
// Configuration is read from config.yaml inside the data directory and can be
// overridden by environment variables prefixed AUDIOSILO_SIDECARS_. On first run
// the file does not exist and Load returns a Config populated with defaults;
// callers persist it via Save. Secrets (API keys, PATs) are NEVER stored here -
// they live in internal/secrets (OS keychain / 0600 fallback).
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"gopkg.in/yaml.v3"
)

// FileName is the config file inside the data directory.
const FileName = "config.yaml"

// DefaultListen is the default bind address: loopback only, so the daemon is not
// exposed to the network unless the operator opts in.
const DefaultListen = "127.0.0.1:8090"

// DefaultConcurrency is the default number of parallel agent slots (Lane B),
// honoured by the scheduler's agent lane (M1).
const DefaultConcurrency = 2

// DefaultTimeoutMinutes is the default per-invocation agent timeout (M5): the wall
// clock any single agent CLI run (claude/codex) is allowed before the runner kills
// it. Applied per invocation, not per stage or per book.
const DefaultTimeoutMinutes = 60

// DefaultBookBudgetUSD is the default per-book agent spend cap: an agent stage parks the
// book budget_exceeded (everything recorded) once its summed cost reaches this, before
// spending more - the backstop for a pathological book (one real 90-chapter book burned
// $62 before parking). Set a very large value in config.yaml to effectively disable the
// guard. Restart-to-apply, like the rest of agent.*.
const DefaultBookBudgetUSD = 75.0

const (
	DefaultSupervisorIntervalSeconds = 30
	DefaultSupervisorStaleMinutes    = 20
	DefaultSupervisorNoProgressMins  = 30
	DefaultSupervisorMaxStageMinutes = 180
	DefaultSupervisorMaxAttempts     = 3
	DefaultSupervisorMaxRepeats      = 2
	DefaultSupervisorAttemptGrowth   = 3.0
	DefaultSupervisorMaxModelCalls   = 1
	DefaultSupervisorMaxTurns        = 8
	DefaultSupervisorTimeoutSeconds  = 90
	DefaultSupervisorCallsPerHour    = 4
	DefaultSupervisorBookBudgetUSD   = 2.0
	DefaultSupervisorBatchBudgetUSD  = 10.0
)

// Agent backend selector values. An empty backend means "auto" (claude when
// detectable, else codex, else unavailable); the two explicit values force one
// runner.
const (
	AgentBackendClaude = agent.IDClaude
	AgentBackendCodex  = agent.IDCodex
)

// DefaultMetadataBaseURL is the community metadata API the coverage/lookup client
// queries. Overridable (env / config) so tests can point at a local httptest.
const DefaultMetadataBaseURL = "https://meta.audiosilo.app"

// Contribution mode selector values: how the contributing stage publishes a book's
// sidecars. issue opens prefilled intake issues (the meta repo's bot composes the
// PR); pr forks + opens a direct PR; local exports to <data>/export with no network.
const (
	ContributionModeIssue = "issue"
	ContributionModePR    = "pr"
	ContributionModeLocal = "local"
)

// Contribution section defaults.
const (
	DefaultContributionMode        = ContributionModeIssue
	DefaultContributionRepo        = "KodeStar/audiosilo-meta"
	DefaultContributionAutoPurge   = true
	DefaultContributionPollMinutes = 10
	// DefaultContributionAPIBaseURL is the GitHub REST API root the contributing
	// stage and intake poller talk to. Overridable (config/env) for tests (point at
	// an httptest fake) and GitHub Enterprise (a self-hosted API host).
	DefaultContributionAPIBaseURL = "https://api.github.com"
)

// DefaultAutoDownload is the tools.auto_download default: when a tool is not found
// locally (explicit path -> next to the binary -> $PATH), fetch a static build
// into <data>/tools rather than failing (HTTPS from pinned hosts, self-checked by
// running -version; no digest pinning).
const DefaultAutoDownload = true

// ASR backend selector values. DefaultASRBackend ("auto") lets the daemon pick:
// mlx-whisper on Apple Silicon with python3, else whisper-cpp when a whisper-cli
// binary is found, else ASR is unavailable.
const (
	ASRBackendAuto       = "auto"
	ASRBackendMLXWhisper = "mlx-whisper"
	ASRBackendWhisperCpp = "whisper-cpp"
	DefaultASRBackend    = ASRBackendAuto
	DefaultASRLanguage   = "en"
)

// ASRConfig holds automatic-speech-recognition settings, live as of M3a. Backend
// selects/forces an ASR backend; Model/Language fall back to each backend's
// default when empty (mlx-community/whisper-large-v3-turbo or
// ggml-large-v3-turbo.bin; "en"). WhisperCLIPath explicitly locates the
// whisper.cpp binary; when it is empty and the binary is not found locally, the
// whisper-cpp backend auto-downloads a prebuilt whisper-cli from the pinned release
// (gated by tools.auto_download, on a supported platform). The device is NOT a
// config knob (no backend honors an override yet) - /system reports the device the
// resolved backend actually detected; a device override can return here once a
// backend honors it.
//
// Changing asr.backend (or the tool paths) takes effect only on a daemon RESTART:
// the backend is resolved once at startup (asr.Select in server.go). This is unlike
// cors_origins, which the API re-reads live per request.
type ASRConfig struct {
	Backend        string `yaml:"backend"`          // "auto" | "mlx-whisper" | "whisper-cpp"
	Model          string `yaml:"model"`            // "" defaults per backend
	Language       string `yaml:"language"`         // "" defaults to "en"
	WhisperCLIPath string `yaml:"whisper_cli_path"` // explicit whisper-cli location
}

// AgentConfig holds the agent-runner settings (live as of M5). Backend selects the
// headless CLI runner; empty = auto. ClaudePath/CodexPath explicitly locate the CLI
// binaries (empty = $PATH lookup). TimeoutMinutes bounds a single invocation. Model
// routing is config, never hardcoded: Claude/OpenAI map a pipeline stage name to a
// model name, so no vendor model id ever appears in code. An empty OpenAI map is
// intentional - an unset codex model means "use the codex CLI's default model"; we
// never invent an OpenAI model id.
//
// Changing any agent field takes effect only on a daemon RESTART (the runner is
// resolved once at startup, like asr.backend), unlike cors_origins which the API
// re-reads live per request.
type AgentConfig struct {
	Backend        string `yaml:"backend"`         // "" (auto) | "claude" | "codex"
	Concurrency    int    `yaml:"concurrency"`     // parallel agent slots (Lane B)
	ClaudePath     string `yaml:"claude_path"`     // explicit claude CLI location; "" = $PATH
	CodexPath      string `yaml:"codex_path"`      // explicit codex CLI location; "" = $PATH
	TimeoutMinutes int    `yaml:"timeout_minutes"` // per-invocation wall-clock cap
	// BookBudgetUSD caps total agent spend per book: a stage parks the book
	// budget_exceeded once its summed cost reaches this (0 -> the default; set a very
	// large value to effectively disable). Restart-to-apply, like the rest of agent.*.
	BookBudgetUSD float64           `yaml:"book_budget_usd"`
	Claude        map[string]string `yaml:"claude"` // agent-stage name -> model
	OpenAI        map[string]string `yaml:"openai"` // agent-stage name -> model (empty = codex default)
}

// SupervisorConfig controls the bounded orchestration monitor. Enabled defaults on,
// but AutomaticActions, ModelAssisted and backend failover default off. Thus an old
// config gains cheap incident visibility without changing production scheduling.
type SupervisorConfig struct {
	Enabled               bool   `yaml:"enabled"`
	AutomaticActions      bool   `yaml:"automatic_actions"`
	ModelAssisted         bool   `yaml:"model_assisted"`
	ModelAutomaticActions bool   `yaml:"model_automatic_actions"`
	AllowBackendFailover  bool   `yaml:"allow_backend_failover"`
	FallbackBackend       string `yaml:"fallback_backend"`
	FallbackModel         string `yaml:"fallback_model"`

	IntervalSeconds     int     `yaml:"interval_seconds"`
	StaleMinutes        int     `yaml:"stale_heartbeat_minutes"`
	NoProgressMinutes   int     `yaml:"no_progress_minutes"`
	MaxStageMinutes     int     `yaml:"max_stage_minutes"`
	MaxAttempts         int     `yaml:"max_attempts"`
	MaxErrorRepeats     int     `yaml:"max_error_repeats"`
	MaxStageTokens      int64   `yaml:"max_stage_tokens"`
	MaxStageCostUSD     float64 `yaml:"max_stage_cost_usd"`
	AttemptGrowthFactor float64 `yaml:"attempt_growth_factor"`

	ModelBackend          string  `yaml:"model_backend"`
	Model                 string  `yaml:"model"`
	MaxModelCalls         int     `yaml:"max_model_calls"`
	MaxTurns              int     `yaml:"max_turns"`
	TimeoutSeconds        int     `yaml:"timeout_seconds"`
	InvocationsPerHour    int     `yaml:"invocations_per_hour"`
	PerBookBudgetUSD      float64 `yaml:"per_book_budget_usd"`
	OverallBatchBudgetUSD float64 `yaml:"overall_batch_budget_usd"`
}

// ContributionConfig controls the contributing stage + the intake poller (M7). Mode
// selects how a book's sidecars are published; Repo is the upstream meta repository
// (owner/name); AutoPurge reclaims a book's scratch once it reaches done; PollMinutes
// is the interval at which open contributions are polled for their intake-PR status.
//
// Changing any contribution field takes effect only on a daemon RESTART (the stage
// and poller are wired once at startup, like asr.backend and agent.*), unlike
// cors_origins which the API re-reads live per request.
type ContributionConfig struct {
	Mode        string `yaml:"mode"`         // "issue" | "pr" | "local"
	Repo        string `yaml:"repo"`         // upstream meta repo, owner/name
	AutoPurge   bool   `yaml:"auto_purge"`   // purge scratch when a book reaches done
	PollMinutes int    `yaml:"poll_minutes"` // open-contribution poll interval (>= 1)
	// APIBaseURL is the GitHub REST API root (absolute http(s) URL). Empty defaults
	// to https://api.github.com. Overridable for tests (an httptest fake) and GitHub
	// Enterprise (a self-hosted API host).
	APIBaseURL string `yaml:"api_base_url"`
}

// MetadataConfig points the coverage/lookup client at the community metadata API.
type MetadataConfig struct {
	// BaseURL is the metadata API root. Must be an absolute http(s) URL. Empty
	// disables coverage lookups (the scan still runs; books are marked unknown).
	BaseURL string `yaml:"base_url"`
}

// ToolsConfig locates the external media tools (ffmpeg + ffprobe) the audio
// stages and the folder scan use. An empty path means "resolve automatically":
// next to the daemon binary, then $PATH, and finally (when AutoDownload) an
// on-demand download into <data>/tools. This is the SINGLE source of truth for
// tool locations - the folder scan consumes the RESOLVED ffprobe path, not a
// separate knob.
type ToolsConfig struct {
	// FFmpegPath is an explicit ffmpeg binary (path or PATH-resolvable name). ""
	// resolves automatically. ffmpeg drives the chapter FLAC split.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// FFprobePath is an explicit ffprobe binary. "" resolves automatically.
	// ffprobe drives inspect (format + chapter markers) and scan enrichment.
	FFprobePath string `yaml:"ffprobe_path"`
	// AutoDownload, when true (the default), fetches a static build into
	// <data>/tools when neither an explicit path, a copy next to the binary, nor
	// $PATH turns up the tool. It also gates the whisper-cpp ASR backend's fetch of
	// a prebuilt whisper-cli from the pinned release (internal/toolfetch), so a user
	// on non-Apple hardware gets a working ASR backend with no manual install. Set
	// false for an air-gapped/managed environment.
	AutoDownload bool `yaml:"auto_download"`
}

// Config is the daemon configuration.
type Config struct {
	// Listen is the HTTP bind address (host:port). Defaults to loopback.
	Listen string `yaml:"listen"`
	// CORSOrigins is the allow-list of browser origins permitted to call the API
	// cross-origin (for a separately-deployed UI container). Empty = same-origin
	// only, which is the secure default.
	CORSOrigins []string `yaml:"cors_origins"`
	// LibraryRoots restricts which local directories a scan may target. Empty =
	// allow any local path (the loopback trust model); when non-empty, a scan
	// path must be inside one of these absolute roots. Each entry must be an
	// absolute path.
	LibraryRoots []string `yaml:"library_roots"`
	// Metadata configures the community metadata API client.
	Metadata MetadataConfig `yaml:"metadata"`
	// Tools locates ffmpeg/ffprobe (resolved at startup, see internal/toolfetch).
	// It is the single source of truth for tool paths; the folder scan uses the
	// resolved ffprobe.
	Tools ToolsConfig `yaml:"tools"`
	// ASR and Agent are typed stubs consumed by later milestones (Agent.Concurrency
	// is live in M1).
	ASR   ASRConfig   `yaml:"asr"`
	Agent AgentConfig `yaml:"agent"`
	// Pricing is versioned and operator supplied. Empty rates mean an unavailable
	// estimate, never a zero-cost model call.
	Pricing pricing.Table `yaml:"pricing"`
	// Supervisor is orchestration-only and never appears in the pipeline state table.
	Supervisor SupervisorConfig `yaml:"supervisor"`
	// Contribution configures the contributing stage + intake poller (M7).
	Contribution ContributionConfig `yaml:"contribution"`
}

// Default returns a Config with secure defaults.
func Default() Config {
	return Config{
		Listen:       DefaultListen,
		CORSOrigins:  []string{},
		LibraryRoots: []string{},
		Metadata:     MetadataConfig{BaseURL: DefaultMetadataBaseURL},
		Tools:        ToolsConfig{AutoDownload: DefaultAutoDownload},
		ASR:          ASRConfig{Backend: DefaultASRBackend, Language: DefaultASRLanguage},
		Agent: AgentConfig{
			Concurrency:    DefaultConcurrency,
			TimeoutMinutes: DefaultTimeoutMinutes,
			BookBudgetUSD:  DefaultBookBudgetUSD,
			Claude:         defaultClaudeModels(),
			OpenAI:         map[string]string{},
		},
		Pricing: pricing.Table{Version: "unconfigured-v1", Rates: map[string]pricing.Rate{}},
		Supervisor: SupervisorConfig{
			Enabled:               true,
			IntervalSeconds:       DefaultSupervisorIntervalSeconds,
			StaleMinutes:          DefaultSupervisorStaleMinutes,
			NoProgressMinutes:     DefaultSupervisorNoProgressMins,
			MaxStageMinutes:       DefaultSupervisorMaxStageMinutes,
			MaxAttempts:           DefaultSupervisorMaxAttempts,
			MaxErrorRepeats:       DefaultSupervisorMaxRepeats,
			AttemptGrowthFactor:   DefaultSupervisorAttemptGrowth,
			MaxModelCalls:         DefaultSupervisorMaxModelCalls,
			MaxTurns:              DefaultSupervisorMaxTurns,
			TimeoutSeconds:        DefaultSupervisorTimeoutSeconds,
			InvocationsPerHour:    DefaultSupervisorCallsPerHour,
			PerBookBudgetUSD:      DefaultSupervisorBookBudgetUSD,
			OverallBatchBudgetUSD: DefaultSupervisorBatchBudgetUSD,
		},
		Contribution: ContributionConfig{
			Mode:        DefaultContributionMode,
			Repo:        DefaultContributionRepo,
			AutoPurge:   DefaultContributionAutoPurge,
			PollMinutes: DefaultContributionPollMinutes,
			APIBaseURL:  DefaultContributionAPIBaseURL,
		},
	}
}

// defaultClaudeModels seeds the claude model routing map: which model runs each
// agent stage under the claude backend. Cheap stages get sonnet; the load-bearing
// synthesis/audit/fix stages get opus. Model aliases ("sonnet"/"opus"/"haiku") are
// what the claude CLI accepts - never a pinned vendor model id. The openai map is
// intentionally empty (an unset codex model = the codex CLI's default model).
func defaultClaudeModels() map[string]string {
	return map[string]string{
		"markers_normalizing": "sonnet",
		"qa_adjudicating":     "sonnet",
		"spelling_research":   "sonnet",
		"fact_pass":           "sonnet",
		"synthesizing":        "opus",
		"auditing":            "opus",
		"fixing":              "opus",
	}
}

// Load reads config.yaml from dataDir, applies environment overrides, validates,
// and returns the result. A missing file yields defaults (first run).
func Load(dataDir string) (Config, error) {
	cfg := Default()
	path := filepath.Join(dataDir, FileName)
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled data dir
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(raw, &cfg); uerr != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, uerr)
		}
	case errors.Is(err, os.ErrNotExist):
		// First run: keep defaults.
	default:
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	applyEnv(&cfg)
	if cfg.Agent.Concurrency == 0 {
		cfg.Agent.Concurrency = DefaultConcurrency
	}
	if cfg.Agent.TimeoutMinutes == 0 {
		cfg.Agent.TimeoutMinutes = DefaultTimeoutMinutes
	}
	// 0 (absent / an older config) adopts the default budget; an explicit large value is
	// how a user effectively disables the guard, so only the zero sentinel is normalized.
	if cfg.Agent.BookBudgetUSD == 0 {
		cfg.Agent.BookBudgetUSD = DefaultBookBudgetUSD
	}
	normalizeSupervisor(&cfg.Supervisor)
	if cfg.Pricing.Rates == nil {
		cfg.Pricing.Rates = map[string]pricing.Rate{}
	}
	// Normalize ASR defaults so an older/partial config.yaml (or one predating M3a)
	// resolves to a working backend without an explicit edit.
	if strings.TrimSpace(cfg.ASR.Backend) == "" {
		cfg.ASR.Backend = DefaultASRBackend
	}
	if strings.TrimSpace(cfg.ASR.Language) == "" {
		cfg.ASR.Language = DefaultASRLanguage
	}
	if cfg.CORSOrigins == nil {
		cfg.CORSOrigins = []string{}
	}
	if cfg.LibraryRoots == nil {
		cfg.LibraryRoots = []string{}
	}
	// Normalize contribution defaults so a config predating M7 (no contribution
	// section) or with an empty mode/repo/interval resolves to working values. AutoPurge
	// is a bool with no empty sentinel, so an explicit false is honored as-is.
	if strings.TrimSpace(cfg.Contribution.Mode) == "" {
		cfg.Contribution.Mode = DefaultContributionMode
	}
	if strings.TrimSpace(cfg.Contribution.Repo) == "" {
		cfg.Contribution.Repo = DefaultContributionRepo
	}
	if cfg.Contribution.PollMinutes == 0 {
		cfg.Contribution.PollMinutes = DefaultContributionPollMinutes
	}
	if strings.TrimSpace(cfg.Contribution.APIBaseURL) == "" {
		cfg.Contribution.APIBaseURL = DefaultContributionAPIBaseURL
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv overlays AUDIOSILO_SIDECARS_* environment variables onto cfg.
func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_LISTEN"); ok {
		cfg.Listen = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CORS_ORIGINS"); ok {
		cfg.CORSOrigins = splitList(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_LIBRARY_ROOTS"); ok {
		cfg.LibraryRoots = splitList(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_METADATA_BASE_URL"); ok {
		cfg.Metadata.BaseURL = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_TOOLS_FFMPEG_PATH"); ok {
		cfg.Tools.FFmpegPath = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_TOOLS_FFPROBE_PATH"); ok {
		cfg.Tools.FFprobePath = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_TOOLS_AUTO_DOWNLOAD"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Tools.AutoDownload = b
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_BACKEND"); ok {
		cfg.ASR.Backend = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_MODEL"); ok {
		cfg.ASR.Model = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_LANGUAGE"); ok {
		cfg.ASR.Language = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_WHISPER_CLI_PATH"); ok {
		cfg.ASR.WhisperCLIPath = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_BACKEND"); ok {
		cfg.Agent.Backend = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_CONCURRENCY"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Agent.Concurrency = n
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_CLAUDE_PATH"); ok {
		cfg.Agent.ClaudePath = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_CODEX_PATH"); ok {
		cfg.Agent.CodexPath = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_TIMEOUT_MINUTES"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Agent.TimeoutMinutes = n
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_BOOK_BUDGET_USD"); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			cfg.Agent.BookBudgetUSD = f
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_SUPERVISOR_ENABLED"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Supervisor.Enabled = b
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_SUPERVISOR_AUTOMATIC_ACTIONS"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Supervisor.AutomaticActions = b
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_SUPERVISOR_MODEL_ASSISTED"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Supervisor.ModelAssisted = b
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CONTRIBUTION_MODE"); ok {
		cfg.Contribution.Mode = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CONTRIBUTION_REPO"); ok {
		cfg.Contribution.Repo = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CONTRIBUTION_AUTO_PURGE"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Contribution.AutoPurge = b
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CONTRIBUTION_POLL_MINUTES"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Contribution.PollMinutes = n
		}
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_CONTRIBUTION_API_BASE_URL"); ok {
		cfg.Contribution.APIBaseURL = strings.TrimSpace(v)
	}
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func normalizeSupervisor(c *SupervisorConfig) {
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = DefaultSupervisorIntervalSeconds
	}
	if c.StaleMinutes == 0 {
		c.StaleMinutes = DefaultSupervisorStaleMinutes
	}
	if c.NoProgressMinutes == 0 {
		c.NoProgressMinutes = DefaultSupervisorNoProgressMins
	}
	if c.MaxStageMinutes == 0 {
		c.MaxStageMinutes = DefaultSupervisorMaxStageMinutes
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = DefaultSupervisorMaxAttempts
	}
	if c.MaxErrorRepeats == 0 {
		c.MaxErrorRepeats = DefaultSupervisorMaxRepeats
	}
	if c.AttemptGrowthFactor == 0 {
		c.AttemptGrowthFactor = DefaultSupervisorAttemptGrowth
	}
	if c.MaxModelCalls == 0 {
		c.MaxModelCalls = DefaultSupervisorMaxModelCalls
	}
	if c.MaxTurns == 0 {
		c.MaxTurns = DefaultSupervisorMaxTurns
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = DefaultSupervisorTimeoutSeconds
	}
	if c.InvocationsPerHour == 0 {
		c.InvocationsPerHour = DefaultSupervisorCallsPerHour
	}
	if c.PerBookBudgetUSD == 0 {
		c.PerBookBudgetUSD = DefaultSupervisorBookBudgetUSD
	}
	if c.OverallBatchBudgetUSD == 0 {
		c.OverallBatchBudgetUSD = DefaultSupervisorBatchBudgetUSD
	}
}

func validateSupervisor(c SupervisorConfig) error {
	positive := map[string]int{
		"interval_seconds":        c.IntervalSeconds,
		"stale_heartbeat_minutes": c.StaleMinutes,
		"no_progress_minutes":     c.NoProgressMinutes,
		"max_stage_minutes":       c.MaxStageMinutes,
		"max_attempts":            c.MaxAttempts,
		"max_error_repeats":       c.MaxErrorRepeats,
		"max_model_calls":         c.MaxModelCalls,
		"max_turns":               c.MaxTurns,
		"timeout_seconds":         c.TimeoutSeconds,
		"invocations_per_hour":    c.InvocationsPerHour,
	}
	for name, value := range positive {
		if value < 1 {
			return fmt.Errorf("supervisor.%s must be >= 1, got %d", name, value)
		}
	}
	if c.MaxStageTokens < 0 || c.MaxStageCostUSD < 0 || c.PerBookBudgetUSD < 0 || c.OverallBatchBudgetUSD < 0 {
		return errors.New("supervisor token and cost limits must be >= 0")
	}
	if c.AttemptGrowthFactor < 1 {
		return errors.New("supervisor.attempt_growth_factor must be >= 1")
	}
	for name, backend := range map[string]string{"model_backend": c.ModelBackend, "fallback_backend": c.FallbackBackend} {
		switch strings.TrimSpace(backend) {
		case "", AgentBackendClaude, AgentBackendCodex:
		default:
			return fmt.Errorf("supervisor.%s %q must be %q, %q, or empty", name, backend, AgentBackendClaude, AgentBackendCodex)
		}
	}
	if c.ModelAutomaticActions && !c.ModelAssisted {
		return errors.New("supervisor.model_automatic_actions requires model_assisted")
	}
	if c.AllowBackendFailover && strings.TrimSpace(c.FallbackBackend) == "" {
		return errors.New("supervisor.allow_backend_failover requires fallback_backend")
	}
	return nil
}

// Validate reports whether the configuration is internally consistent.
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("invalid listen address %q: %w", c.Listen, err)
	}
	if c.Agent.Concurrency < 1 {
		return fmt.Errorf("agent.concurrency must be >= 1, got %d", c.Agent.Concurrency)
	}
	switch c.Agent.Backend {
	case "", AgentBackendClaude, AgentBackendCodex:
	default:
		return fmt.Errorf("agent.backend %q must be %q, %q, or empty (auto)",
			c.Agent.Backend, AgentBackendClaude, AgentBackendCodex)
	}
	if c.Agent.TimeoutMinutes < 1 {
		return fmt.Errorf("agent.timeout_minutes must be >= 1, got %d", c.Agent.TimeoutMinutes)
	}
	if c.Agent.BookBudgetUSD < 0 {
		return fmt.Errorf("agent.book_budget_usd must be >= 0, got %v", c.Agent.BookBudgetUSD)
	}
	if err := validateSupervisor(c.Supervisor); err != nil {
		return err
	}
	if strings.TrimSpace(c.Pricing.Version) == "" {
		return errors.New("pricing.version is required")
	}
	for key, rate := range c.Pricing.Rates {
		if !strings.Contains(key, "/") {
			return fmt.Errorf("pricing.rates key %q must be backend/model", key)
		}
		if rate.InputUSDPerMillion < 0 || rate.OutputUSDPerMillion < 0 || rate.CachedInputUSDPerMillion < 0 {
			return fmt.Errorf("pricing.rates[%q] values must be >= 0", key)
		}
	}
	// Model-map keys must name agent-lane stages; a typo would silently never route.
	for key := range c.Agent.Claude {
		if !state.IsAgent(state.State(key)) {
			return fmt.Errorf("agent.claude has key %q, which is not an agent stage", key)
		}
	}
	for key := range c.Agent.OpenAI {
		if !state.IsAgent(state.State(key)) {
			return fmt.Errorf("agent.openai has key %q, which is not an agent stage", key)
		}
	}
	for _, o := range c.CORSOrigins {
		if err := validateOrigin(o); err != nil {
			return err
		}
	}
	for _, root := range c.LibraryRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("library_roots entry %q must be an absolute path", root)
		}
	}
	if c.Metadata.BaseURL != "" {
		u, err := url.Parse(c.Metadata.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("metadata.base_url %q must be an absolute http(s) URL", c.Metadata.BaseURL)
		}
	}
	switch c.ASR.Backend {
	case "", ASRBackendAuto, ASRBackendMLXWhisper, ASRBackendWhisperCpp:
	default:
		return fmt.Errorf("asr.backend %q must be one of %q, %q, or %q",
			c.ASR.Backend, ASRBackendAuto, ASRBackendMLXWhisper, ASRBackendWhisperCpp)
	}
	switch c.Contribution.Mode {
	case ContributionModeIssue, ContributionModePR, ContributionModeLocal:
	default:
		return fmt.Errorf("contribution.mode %q must be %q, %q, or %q",
			c.Contribution.Mode, ContributionModeIssue, ContributionModePR, ContributionModeLocal)
	}
	if err := validateRepo(c.Contribution.Repo); err != nil {
		return err
	}
	if c.Contribution.PollMinutes < 1 {
		return fmt.Errorf("contribution.poll_minutes must be >= 1, got %d", c.Contribution.PollMinutes)
	}
	if u, err := url.Parse(c.Contribution.APIBaseURL); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("contribution.api_base_url %q must be an absolute http(s) URL", c.Contribution.APIBaseURL)
	}
	return nil
}

// validateRepo enforces the GitHub owner/name shape: exactly one slash, both parts
// non-empty, and no whitespace anywhere (it is interpolated into REST paths).
func validateRepo(repo string) error {
	if strings.ContainsAny(repo, " \t\r\n") {
		return fmt.Errorf("contribution.repo %q must not contain whitespace", repo)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("contribution.repo %q must be owner/name", repo)
	}
	return nil
}

// validateOrigin rejects anything that is not a bare http(s) origin
// (scheme://host[:port], no path/query/fragment) - the exact form a browser
// sends in an Origin header and the CORS layer compares against.
func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil {
		return fmt.Errorf("invalid cors origin %q: %w", o, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid cors origin %q: scheme must be http or https", o)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid cors origin %q: missing host", o)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid cors origin %q: must be scheme://host[:port] with no path", o)
	}
	return nil
}

// Save writes cfg to config.yaml in dataDir with 0600 permissions.
func Save(dataDir string, cfg Config) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := filepath.Join(dataDir, FileName)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
