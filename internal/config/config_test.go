package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if cfg.Agent.Concurrency != DefaultConcurrency {
		t.Errorf("concurrency = %d, want %d", cfg.Agent.Concurrency, DefaultConcurrency)
	}
	if cfg.Agent.BookBudgetUSD != DefaultBookBudgetUSD {
		t.Errorf("book_budget_usd default = %v, want %v", cfg.Agent.BookBudgetUSD, DefaultBookBudgetUSD)
	}
	if cfg.CORSOrigins == nil {
		t.Error("CORSOrigins should be non-nil empty slice")
	}
}

func TestBookBudgetNormalizeAndOverride(t *testing.T) {
	// A config with book_budget_usd unset (0) adopts the default on Load ...
	dir := t.TempDir()
	in := Default()
	in.Agent.BookBudgetUSD = 0
	if err := Save(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent.BookBudgetUSD != DefaultBookBudgetUSD {
		t.Errorf("zero budget normalized to %v, want default %v", out.Agent.BookBudgetUSD, DefaultBookBudgetUSD)
	}
	// ... and an explicit (large) value round-trips untouched, so a user can effectively
	// disable the guard.
	in.Agent.BookBudgetUSD = 100000
	if err := Save(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent.BookBudgetUSD != 100000 {
		t.Errorf("explicit budget = %v, want 100000", out.Agent.BookBudgetUSD)
	}
	// The env override applies too.
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_BOOK_BUDGET_USD", "42.5")
	out, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent.BookBudgetUSD != 42.5 {
		t.Errorf("env override budget = %v, want 42.5", out.Agent.BookBudgetUSD)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Default()
	in.Listen = "0.0.0.0:9000"
	in.CORSOrigins = []string{"http://localhost:5173"}
	in.ASR.Backend = "whisper-cpp"
	in.Agent.Backend = "claude"
	in.Agent.Concurrency = 4
	in.Agent.Claude = map[string]string{"fact_pass": "sonnet", "synthesizing": "opus"}
	in.Agent.OpenAI = map[string]string{"auditing": "gpt-x"}
	in.Agent.TimeoutMinutes = 90
	in.Agent.ClaudePath = "/opt/claude"
	in.Agent.CodexPath = "/opt/codex"
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Listen != in.Listen {
		t.Errorf("listen = %q, want %q", out.Listen, in.Listen)
	}
	if len(out.CORSOrigins) != 1 || out.CORSOrigins[0] != "http://localhost:5173" {
		t.Errorf("cors = %v", out.CORSOrigins)
	}
	if out.ASR.Backend != "whisper-cpp" {
		t.Errorf("asr backend = %q", out.ASR.Backend)
	}
	if out.Agent.Concurrency != 4 {
		t.Errorf("concurrency = %d", out.Agent.Concurrency)
	}
	if out.Agent.Claude["synthesizing"] != "opus" || out.Agent.Claude["fact_pass"] != "sonnet" {
		t.Errorf("claude map round-trip = %v", out.Agent.Claude)
	}
	if out.Agent.OpenAI["auditing"] != "gpt-x" {
		t.Errorf("openai map round-trip = %v", out.Agent.OpenAI)
	}
	if out.Agent.TimeoutMinutes != 90 {
		t.Errorf("timeout_minutes = %d, want 90", out.Agent.TimeoutMinutes)
	}
	if out.Agent.ClaudePath != "/opt/claude" || out.Agent.CodexPath != "/opt/codex" {
		t.Errorf("agent paths = %q,%q", out.Agent.ClaudePath, out.Agent.CodexPath)
	}
}

func TestSaveWritesMode0600(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perm = %o, want 600", perm)
	}
}

func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_LISTEN", "127.0.0.1:7777")
	t.Setenv("AUDIOSILO_SIDECARS_CORS_ORIGINS", "http://a.example, https://b.example ")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_CONCURRENCY", "3")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7777" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[1] != "https://b.example" {
		t.Errorf("cors = %v", cfg.CORSOrigins)
	}
	if cfg.Agent.Concurrency != 3 {
		t.Errorf("concurrency = %d", cfg.Agent.Concurrency)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]Config{
		"bad listen":       {Listen: "not-a-host-port", Agent: AgentConfig{Concurrency: 1}},
		"zero concurrency": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 0}},
		"origin with path": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"http://x.example/foo"}},
		"origin no scheme": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"x.example"}},
		"origin ftp":       {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"ftp://x.example"}},
		"bad asr backend":  {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, ASR: ASRConfig{Backend: "faster-whisper"}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}
}

func TestValidateAgentFields(t *testing.T) {
	// Each violation (built on a valid Default base) must be rejected.
	bad := map[string]func(*Config){
		"bad backend":          func(c *Config) { c.Agent.Backend = "gemini" },
		"zero timeout":         func(c *Config) { c.Agent.TimeoutMinutes = 0 },
		"negative timeout":     func(c *Config) { c.Agent.TimeoutMinutes = -1 },
		"non-agent claude key": func(c *Config) { c.Agent.Claude = map[string]string{"splitting": "sonnet"} },
		"non-agent openai key": func(c *Config) { c.Agent.OpenAI = map[string]string{"asr": "gpt-x"} },
		"unknown claude key":   func(c *Config) { c.Agent.Claude = map[string]string{"not_a_stage": "sonnet"} },
		"negative budget":      func(c *Config) { c.Agent.BookBudgetUSD = -1 },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}

	// Valid: explicit backend, agent-stage model keys, positive timeout.
	good := Default()
	good.Agent.Backend = AgentBackendClaude
	good.Agent.TimeoutMinutes = 1
	good.Agent.Claude = map[string]string{"fact_pass": "sonnet", "auditing": "opus"}
	good.Agent.OpenAI = map[string]string{"synthesizing": "gpt-x"}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for valid agent config", err)
	}
}

func TestDefaultSeedsClaudeModels(t *testing.T) {
	cfg := Default()
	if cfg.Agent.TimeoutMinutes != DefaultTimeoutMinutes {
		t.Errorf("timeout_minutes default = %d, want %d", cfg.Agent.TimeoutMinutes, DefaultTimeoutMinutes)
	}
	want := map[string]string{
		"markers_normalizing": "sonnet",
		"qa_adjudicating":     "sonnet",
		"spelling_research":   "sonnet",
		"fact_pass":           "sonnet",
		"synthesizing":        "opus",
		"auditing":            "opus",
		"fixing":              "opus",
	}
	for k, v := range want {
		if cfg.Agent.Claude[k] != v {
			t.Errorf("claude[%q] = %q, want %q", k, cfg.Agent.Claude[k], v)
		}
	}
	if len(cfg.Agent.Claude) != len(want) {
		t.Errorf("claude map size = %d, want %d", len(cfg.Agent.Claude), len(want))
	}
	if cfg.Agent.OpenAI == nil || len(cfg.Agent.OpenAI) != 0 {
		t.Errorf("openai map = %v, want empty non-nil", cfg.Agent.OpenAI)
	}
	// Every seeded key must be an agent stage (validation would reject otherwise).
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default() config invalid: %v", err)
	}
}

func TestAgentEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_BACKEND", "codex")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_CLAUDE_PATH", "/usr/local/bin/claude")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_CODEX_PATH", "/usr/local/bin/codex")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_TIMEOUT_MINUTES", "45")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Backend != "codex" {
		t.Errorf("backend = %q, want codex", cfg.Agent.Backend)
	}
	if cfg.Agent.ClaudePath != "/usr/local/bin/claude" || cfg.Agent.CodexPath != "/usr/local/bin/codex" {
		t.Errorf("agent paths = %q,%q", cfg.Agent.ClaudePath, cfg.Agent.CodexPath)
	}
	if cfg.Agent.TimeoutMinutes != 45 {
		t.Errorf("timeout_minutes = %d, want 45", cfg.Agent.TimeoutMinutes)
	}
}

func TestContributionDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Contribution.Mode != DefaultContributionMode {
		t.Errorf("mode = %q, want %q", cfg.Contribution.Mode, DefaultContributionMode)
	}
	if cfg.Contribution.Repo != DefaultContributionRepo {
		t.Errorf("repo = %q, want %q", cfg.Contribution.Repo, DefaultContributionRepo)
	}
	if !cfg.Contribution.AutoPurge {
		t.Error("auto_purge should default to true")
	}
	if cfg.Contribution.PollMinutes != DefaultContributionPollMinutes {
		t.Errorf("poll_minutes = %d, want %d", cfg.Contribution.PollMinutes, DefaultContributionPollMinutes)
	}
	if cfg.Contribution.APIBaseURL != DefaultContributionAPIBaseURL {
		t.Errorf("api_base_url = %q, want %q", cfg.Contribution.APIBaseURL, DefaultContributionAPIBaseURL)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default() contribution invalid: %v", err)
	}
}

func TestContributionValidation(t *testing.T) {
	bad := map[string]func(*Config){
		"bad mode":         func(c *Config) { c.Contribution.Mode = "email" },
		"repo no slash":    func(c *Config) { c.Contribution.Repo = "audiosilo-meta" },
		"repo empty owner": func(c *Config) { c.Contribution.Repo = "/audiosilo-meta" },
		"repo empty name":  func(c *Config) { c.Contribution.Repo = "KodeStar/" },
		"repo two slashes": func(c *Config) { c.Contribution.Repo = "a/b/c" },
		"repo whitespace":  func(c *Config) { c.Contribution.Repo = "Kode Star/meta" },
		"poll zero":        func(c *Config) { c.Contribution.PollMinutes = 0 },
		"poll negative":    func(c *Config) { c.Contribution.PollMinutes = -3 },
		"api base not url": func(c *Config) { c.Contribution.APIBaseURL = "not-a-url" },
		"api base ftp":     func(c *Config) { c.Contribution.APIBaseURL = "ftp://api.example" },
		"api base no host": func(c *Config) { c.Contribution.APIBaseURL = "https://" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}
	// Valid non-default: pr mode, a custom owner/name repo, poll interval 1, an
	// enterprise-style api base URL.
	good := Default()
	good.Contribution.Mode = ContributionModePR
	good.Contribution.Repo = "acme/meta"
	good.Contribution.PollMinutes = 1
	good.Contribution.APIBaseURL = "https://github.acme.com/api/v3"
	if err := good.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for valid contribution", err)
	}
}

func TestContributionEnvOverridesAndRoundTrip(t *testing.T) {
	// Round-trip through Save/Load, including an explicit auto_purge=false.
	dir := t.TempDir()
	in := Default()
	in.Contribution.Mode = ContributionModePR
	in.Contribution.Repo = "acme/meta"
	in.Contribution.AutoPurge = false
	in.Contribution.PollMinutes = 30
	in.Contribution.APIBaseURL = "https://github.acme.com/api/v3"
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Contribution.Mode != ContributionModePR || out.Contribution.Repo != "acme/meta" ||
		out.Contribution.AutoPurge || out.Contribution.PollMinutes != 30 ||
		out.Contribution.APIBaseURL != "https://github.acme.com/api/v3" {
		t.Errorf("round-trip = %+v", out.Contribution)
	}

	// Env overrides take precedence, including auto_purge=false and a new interval.
	envDir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_MODE", "local")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_REPO", "org/repo")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_AUTO_PURGE", "false")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_POLL_MINUTES", "25")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_API_BASE_URL", "http://127.0.0.1:9999")
	cfg, err := Load(envDir)
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if cfg.Contribution.Mode != "local" || cfg.Contribution.Repo != "org/repo" ||
		cfg.Contribution.AutoPurge || cfg.Contribution.PollMinutes != 25 ||
		cfg.Contribution.APIBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("env overrides = %+v", cfg.Contribution)
	}
}

func TestContributionNormalizesEmpty(t *testing.T) {
	// A config file with an empty contribution mode/repo and a zero interval normalizes
	// to defaults on Load (a section predating M7 has none of these keys at all).
	dir := t.TempDir()
	in := Default()
	in.Contribution.Mode = ""
	in.Contribution.Repo = ""
	in.Contribution.PollMinutes = 0
	in.Contribution.APIBaseURL = ""
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Contribution.Mode != DefaultContributionMode ||
		cfg.Contribution.Repo != DefaultContributionRepo ||
		cfg.Contribution.PollMinutes != DefaultContributionPollMinutes ||
		cfg.Contribution.APIBaseURL != DefaultContributionAPIBaseURL {
		t.Errorf("normalization = %+v", cfg.Contribution)
	}
}

func TestValidateAcceptsGoodOrigins(t *testing.T) {
	cfg := Default()
	cfg.CORSOrigins = []string{"http://localhost:5173", "https://ui.example.com"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestDefaultsForNewFields(t *testing.T) {
	cfg := Default()
	if cfg.Metadata.BaseURL != DefaultMetadataBaseURL {
		t.Errorf("metadata base_url = %q, want %q", cfg.Metadata.BaseURL, DefaultMetadataBaseURL)
	}
	if !cfg.Tools.AutoDownload {
		t.Error("tools.auto_download should default to true")
	}
	if cfg.Tools.FFmpegPath != "" || cfg.Tools.FFprobePath != "" {
		t.Errorf("tool paths default = %q,%q, want empty (auto-resolve)", cfg.Tools.FFmpegPath, cfg.Tools.FFprobePath)
	}
	if cfg.LibraryRoots == nil {
		t.Error("library_roots should be non-nil empty slice")
	}
	if cfg.ASR.Backend != ASRBackendAuto {
		t.Errorf("asr.backend default = %q, want %q", cfg.ASR.Backend, ASRBackendAuto)
	}
	if cfg.ASR.Language != DefaultASRLanguage {
		t.Errorf("asr.language default = %q, want %q", cfg.ASR.Language, DefaultASRLanguage)
	}
}

func TestASRConfigEnvAndNormalization(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_ASR_BACKEND", "whisper-cpp")
	t.Setenv("AUDIOSILO_SIDECARS_ASR_MODEL", "ggml-tiny.bin")
	t.Setenv("AUDIOSILO_SIDECARS_ASR_LANGUAGE", "de")
	t.Setenv("AUDIOSILO_SIDECARS_ASR_WHISPER_CLI_PATH", "/opt/whisper-cli")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ASR.Backend != "whisper-cpp" || cfg.ASR.Model != "ggml-tiny.bin" ||
		cfg.ASR.Language != "de" || cfg.ASR.WhisperCLIPath != "/opt/whisper-cli" {
		t.Errorf("asr env overrides not applied: %+v", cfg.ASR)
	}
}

func TestASRBackendNormalizesEmpty(t *testing.T) {
	// A config file with an empty asr.backend normalizes to "auto" on Load.
	dir := t.TempDir()
	in := Default()
	in.ASR.Backend = ""
	in.ASR.Language = ""
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ASR.Backend != ASRBackendAuto || cfg.ASR.Language != DefaultASRLanguage {
		t.Errorf("empty asr backend/language did not normalize: %+v", cfg.ASR)
	}
}

func TestNewFieldsRoundTripAndEnv(t *testing.T) {
	dir := t.TempDir()
	in := Default()
	in.LibraryRoots = []string{"/srv/audiobooks"}
	in.Metadata.BaseURL = "http://localhost:9999"
	in.Tools.FFmpegPath = "/opt/ffmpeg"
	in.Tools.AutoDownload = false
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out.LibraryRoots) != 1 || out.LibraryRoots[0] != "/srv/audiobooks" {
		t.Errorf("library_roots = %v", out.LibraryRoots)
	}
	if out.Metadata.BaseURL != "http://localhost:9999" || out.Tools.FFmpegPath != "/opt/ffmpeg" || out.Tools.AutoDownload {
		t.Errorf("metadata/tools round-trip: %+v %+v", out.Metadata, out.Tools)
	}

	// Env overrides.
	t.Setenv("AUDIOSILO_SIDECARS_LIBRARY_ROOTS", "/a, /b ")
	t.Setenv("AUDIOSILO_SIDECARS_METADATA_BASE_URL", "https://meta.example")
	t.Setenv("AUDIOSILO_SIDECARS_TOOLS_FFPROBE_PATH", "/usr/bin/ffprobe")
	t.Setenv("AUDIOSILO_SIDECARS_TOOLS_AUTO_DOWNLOAD", "true")
	env, err := Load(dir)
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if len(env.LibraryRoots) != 2 || env.LibraryRoots[1] != "/b" {
		t.Errorf("env library_roots = %v", env.LibraryRoots)
	}
	if env.Metadata.BaseURL != "https://meta.example" || env.Tools.FFprobePath != "/usr/bin/ffprobe" || !env.Tools.AutoDownload {
		t.Errorf("env metadata/tools: %+v %+v", env.Metadata, env.Tools)
	}
}

func TestValidateRejectsBadNewFields(t *testing.T) {
	rel := Default()
	rel.LibraryRoots = []string{"relative/path"}
	if err := rel.Validate(); err == nil {
		t.Error("relative library_roots entry should be rejected")
	}
	bad := Default()
	bad.Metadata.BaseURL = "not-a-url"
	if err := bad.Validate(); err == nil {
		t.Error("non-absolute metadata.base_url should be rejected")
	}
	off := Default()
	off.Metadata.BaseURL = "" // disabled is valid
	if err := off.Validate(); err != nil {
		t.Errorf("empty metadata.base_url should be valid (disabled): %v", err)
	}
}
