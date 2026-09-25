package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
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
	cap := cfg.Agent.Capacity()
	if cap.QueueConcurrency != DefaultAgentQueueConcurrency || cap.MaxAgentsPerBook != DefaultMaxAgentsPerBook || cap.GlobalInvocations != 6 || cap.Legacy {
		t.Errorf("capacity = %+v", cap)
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
	in.Agent.QueueConcurrency = 0
	in.Agent.MaxAgentsPerBook = 0
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

func TestLegacyConcurrencyNormalizationPreservesGlobalCap(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("listen: 127.0.0.1:8090\nagent:\n  concurrency: 4\n  timeout_minutes: 60\npricing:\n  version: test\nsupervisor: {}\ncontribution: {}\n")
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cap := cfg.Agent.Capacity()
	if !cap.Legacy || cap.QueueConcurrency != 4 || cap.MaxAgentsPerBook != 4 || cap.GlobalInvocations != 4 {
		t.Fatalf("legacy capacity=%+v", cap)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("Load rewrote legacy config: err=%v\n%s", err, after)
	}
}

func TestLegacyZeroConcurrencyKeepsHistoricalDefaulting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("listen: 127.0.0.1:8090\nagent:\n  concurrency: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agent.Capacity(); !got.Legacy || got.GlobalInvocations != DefaultConcurrency {
		t.Fatalf("legacy zero capacity=%+v", got)
	}
}

func TestModernAgentCapacityValidation(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.QueueConcurrency = 3
	cfg.Agent.MaxAgentsPerBook = 3
	if got := cfg.Agent.Capacity().GlobalInvocations; got != 9 {
		t.Fatalf("global=%d", got)
	}
	cfg.Agent.QueueConcurrency = 9
	cfg.Agent.MaxAgentsPerBook = 8
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unreasonable product rejection")
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

func TestSupervisorDefaultsAreMonitorOnlyAndOldConfigsInheritThem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("listen: 127.0.0.1:8090\nagent:\n  concurrency: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Supervisor.Enabled || cfg.Supervisor.AutomaticActions || cfg.Supervisor.ModelAssisted || cfg.Supervisor.ModelAutomaticActions || cfg.Supervisor.AllowBackendFailover {
		t.Fatalf("supervisor defaults are not monitor-only: %+v", cfg.Supervisor)
	}
	if cfg.Pricing.Version == "" || cfg.Pricing.Rates == nil {
		t.Fatalf("pricing defaults unavailable: %+v", cfg.Pricing)
	}
}

func TestSupervisorRejectsUnsafeOrUnpriceableConfiguration(t *testing.T) {
	cases := map[string]func(*Config){
		"model actions without model":    func(c *Config) { c.Supervisor.ModelAutomaticActions = true },
		"model backend without no-tools": func(c *Config) { c.Supervisor.ModelBackend = AgentBackendCodex },
		"failover without fallback":      func(c *Config) { c.Supervisor.AllowBackendFailover = true },
		"negative supervisor budget":     func(c *Config) { c.Supervisor.PerBookBudgetUSD = -1 },
		"unversioned pricing":            func(c *Config) { c.Pricing.Version = "" },
		"negative pricing": func(c *Config) {
			c.Pricing.Rates["codex/*"] = pricing.Rate{InputUSDPerMillion: -1}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestAgentEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_BACKEND", "codex")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_CLAUDE_PATH", "/usr/local/bin/claude")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_CODEX_PATH", "/usr/local/bin/codex")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_TIMEOUT_MINUTES", "45")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_QUEUE_CONCURRENCY", "3")
	t.Setenv("AUDIOSILO_SIDECARS_AGENT_MAX_AGENTS_PER_BOOK", "2")
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
	if got := cfg.Agent.Capacity(); got.QueueConcurrency != 3 || got.MaxAgentsPerBook != 2 || got.GlobalInvocations != 6 || got.Legacy {
		t.Errorf("env capacity=%+v", got)
	}
}

func TestContributionDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Contribution.Mode != DefaultContributionMode {
		t.Errorf("mode = %q, want %q", cfg.Contribution.Mode, DefaultContributionMode)
	}
	if DefaultContributionCoreRepo != "KodeStar/audiosilo-meta" || DefaultContributionCommunityRepo != "KodeStar/audiosilo-meta-community" {
		t.Errorf("default repos = %q / %q", DefaultContributionCoreRepo, DefaultContributionCommunityRepo)
	}
	if cfg.Contribution.CoreRepo != DefaultContributionCoreRepo {
		t.Errorf("core_repo = %q, want %q", cfg.Contribution.CoreRepo, DefaultContributionCoreRepo)
	}
	// The sidecars belong in the community repository since the 2026-08-21 split;
	// the core repository's intake bot refuses them.
	if cfg.Contribution.CommunityRepo != DefaultContributionCommunityRepo {
		t.Errorf("community_repo = %q, want %q", cfg.Contribution.CommunityRepo, DefaultContributionCommunityRepo)
	}
	if cfg.Contribution.Repo != "" {
		t.Errorf("legacy repo = %q, want empty (never seeded)", cfg.Contribution.Repo)
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
		"bad mode":              func(c *Config) { c.Contribution.Mode = "email" },
		"core no slash":         func(c *Config) { c.Contribution.CoreRepo = "audiosilo-meta" },
		"core empty owner":      func(c *Config) { c.Contribution.CoreRepo = "/audiosilo-meta" },
		"core empty name":       func(c *Config) { c.Contribution.CoreRepo = "KodeStar/" },
		"core two slashes":      func(c *Config) { c.Contribution.CoreRepo = "a/b/c" },
		"core whitespace":       func(c *Config) { c.Contribution.CoreRepo = "Kode Star/meta" },
		"core empty":            func(c *Config) { c.Contribution.CoreRepo = "" },
		"community no slash":    func(c *Config) { c.Contribution.CommunityRepo = "audiosilo-meta-community" },
		"community two slashes": func(c *Config) { c.Contribution.CommunityRepo = "a/b/c" },
		"community whitespace":  func(c *Config) { c.Contribution.CommunityRepo = "Kode Star/c" },
		"community empty":       func(c *Config) { c.Contribution.CommunityRepo = "" },
		"poll zero":             func(c *Config) { c.Contribution.PollMinutes = 0 },
		"poll negative":         func(c *Config) { c.Contribution.PollMinutes = -3 },
		"api base not url":      func(c *Config) { c.Contribution.APIBaseURL = "not-a-url" },
		"api base ftp":          func(c *Config) { c.Contribution.APIBaseURL = "ftp://api.example" },
		"api base no host":      func(c *Config) { c.Contribution.APIBaseURL = "https://" },
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
	// Valid non-default: local mode, a custom owner/name repo, poll interval 1, an
	// enterprise-style api base URL.
	good := Default()
	good.Contribution.Mode = ContributionModeLocal
	good.Contribution.CoreRepo = "acme/meta"
	good.Contribution.CommunityRepo = "acme/meta-community"
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
	in.Contribution.Mode = ContributionModeLocal
	in.Contribution.CoreRepo = "acme/meta"
	in.Contribution.CommunityRepo = "acme/meta-community"
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
	if out.Contribution.Mode != ContributionModeLocal || out.Contribution.CoreRepo != "acme/meta" ||
		out.Contribution.CommunityRepo != "acme/meta-community" ||
		out.Contribution.AutoPurge || out.Contribution.PollMinutes != 30 ||
		out.Contribution.APIBaseURL != "https://github.acme.com/api/v3" {
		t.Errorf("round-trip = %+v", out.Contribution)
	}

	// Env overrides take precedence, including auto_purge=false and a new interval.
	envDir := t.TempDir()
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_MODE", "local")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_CORE_REPO", "org/repo")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_COMMUNITY_REPO", "org/community")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_AUTO_PURGE", "false")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_POLL_MINUTES", "25")
	t.Setenv("AUDIOSILO_SIDECARS_CONTRIBUTION_API_BASE_URL", "http://127.0.0.1:9999")
	cfg, err := Load(envDir)
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if cfg.Contribution.Mode != "local" || cfg.Contribution.CoreRepo != "org/repo" ||
		cfg.Contribution.CommunityRepo != "org/community" ||
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
	in.Contribution.CoreRepo = ""
	in.Contribution.CommunityRepo = ""
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
		cfg.Contribution.CoreRepo != DefaultContributionCoreRepo ||
		cfg.Contribution.CommunityRepo != DefaultContributionCommunityRepo ||
		cfg.Contribution.PollMinutes != DefaultContributionPollMinutes ||
		cfg.Contribution.APIBaseURL != DefaultContributionAPIBaseURL {
		t.Errorf("normalization = %+v", cfg.Contribution)
	}
}

// TestContributionLegacySettings: the retired settings keep loading. The pre-split
// `repo` (file or env) is the CORE repo - never the sidecar target - unless an
// explicit core_repo (file or env) is set, which wins; mode "pr" loads as issue.
// Each is reported once through Deprecations.
func TestContributionLegacySettings(t *testing.T) {
	const community = "\n  community_repo: me/community-test\n"
	cases := []struct {
		name          string
		file          string
		env           map[string]string
		wantCore      string
		wantCommunity string
		wantMode      string
		wantNotice    string
	}{
		{name: "file repo, the default", file: "contribution:\n  repo: KodeStar/audiosilo-meta\n",
			wantCore: DefaultContributionCoreRepo, wantNotice: "config.yaml contribution.repo is deprecated"},
		{name: "file repo, custom, with community_repo", file: "contribution:\n  repo: acme/meta" + community,
			wantCore: "acme/meta", wantCommunity: "me/community-test", wantNotice: "config.yaml contribution.repo is deprecated"},
		{name: "file repo loses to file core_repo", file: "contribution:\n  repo: old/meta\n  core_repo: new/meta" + community,
			wantCore: "new/meta", wantCommunity: "me/community-test", wantNotice: "ignored"},
		{name: "env repo, custom, with env community_repo", env: map[string]string{
			"AUDIOSILO_SIDECARS_CONTRIBUTION_REPO": "env/meta", "AUDIOSILO_SIDECARS_CONTRIBUTION_COMMUNITY_REPO": "env/community"},
			wantCore: "env/meta", wantCommunity: "env/community", wantNotice: "AUDIOSILO_SIDECARS_CONTRIBUTION_REPO is deprecated"},
		{name: "env repo loses to file core_repo", file: "contribution:\n  core_repo: file/core" + community,
			env: map[string]string{"AUDIOSILO_SIDECARS_CONTRIBUTION_REPO": "env/meta"}, wantCore: "file/core",
			wantCommunity: "me/community-test", wantNotice: "ignored"},
		{name: "file repo loses to env core_repo", file: "contribution:\n  repo: file/meta" + community,
			env: map[string]string{"AUDIOSILO_SIDECARS_CONTRIBUTION_CORE_REPO": "env/core"}, wantCore: "env/core",
			wantCommunity: "me/community-test", wantNotice: "ignored"},
		{name: "mode pr", file: "contribution:\n  mode: pr\n",
			wantCore: DefaultContributionCoreRepo, wantMode: ContributionModeIssue, wantNotice: `"pr" is retired`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			dir := t.TempDir()
			if c.file != "" {
				if err := os.WriteFile(filepath.Join(dir, FileName), []byte(c.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			wantCommunity := c.wantCommunity
			if wantCommunity == "" {
				wantCommunity = DefaultContributionCommunityRepo
			}
			if cfg.Contribution.CoreRepo != c.wantCore || cfg.Contribution.CommunityRepo != wantCommunity {
				t.Errorf("repos = %q / %q, want %q / %q",
					cfg.Contribution.CoreRepo, cfg.Contribution.CommunityRepo, c.wantCore, wantCommunity)
			}
			if c.wantMode != "" && cfg.Contribution.Mode != c.wantMode {
				t.Errorf("mode = %q, want %q", cfg.Contribution.Mode, c.wantMode)
			}
			if deps := cfg.Deprecations(); len(deps) != 1 || !strings.Contains(deps[0], c.wantNotice) {
				t.Errorf("deprecations = %q, want one containing %q", deps, c.wantNotice)
			}
			// Saving drops the legacy key, so the next Load is notice-free.
			if err := Save(dir, cfg); err != nil {
				t.Fatal(err)
			}
			for k := range c.env {
				_ = os.Unsetenv(k) // re-read without it; t.Setenv restores it after
			}
			again, err := Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(again.Deprecations()) != 0 || again.Contribution.CoreRepo != c.wantCore {
				t.Errorf("reload = %+v deprecations=%q", again.Contribution, again.Deprecations())
			}
		})
	}
}

// TestContributionLegacyCustomRepoNeedsCommunity: a legacy `repo` naming a custom
// (fork / test) repository used to take the sidecars too. It now only sets
// core_repo, so without an explicit community_repo the sidecars would silently go
// to the PUBLIC community repo - Load refuses instead, naming both settings.
func TestContributionLegacyCustomRepoNeedsCommunity(t *testing.T) {
	for name, setup := range map[string]struct {
		file string
		env  map[string]string
	}{
		"file":                {file: "contribution:\n  repo: me/audiosilo-meta-test\n"},
		"env":                 {env: map[string]string{"AUDIOSILO_SIDECARS_CONTRIBUTION_REPO": "me/audiosilo-meta-test"}},
		"file with core_repo": {file: "contribution:\n  repo: me/audiosilo-meta-test\n  core_repo: me/core\n"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range setup.env {
				t.Setenv(k, v)
			}
			dir := t.TempDir()
			if setup.file != "" {
				if err := os.WriteFile(filepath.Join(dir, FileName), []byte(setup.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), "contribution.community_repo") ||
				!strings.Contains(err.Error(), "contribution.repo") || !strings.Contains(err.Error(), "me/audiosilo-meta-test") {
				t.Fatalf("Load err = %v, want a refusal naming contribution.repo and contribution.community_repo", err)
			}
		})
	}
}

// TestContributionNoDeprecationsByDefault: a fresh or modern config reports none.
func TestContributionNoDeprecationsByDefault(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Deprecations()) != 0 {
		t.Errorf("deprecations = %q, want none", cfg.Deprecations())
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
