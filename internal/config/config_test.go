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
	if cfg.CORSOrigins == nil {
		t.Error("CORSOrigins should be non-nil empty slice")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Default()
	in.Listen = "0.0.0.0:9000"
	in.CORSOrigins = []string{"http://localhost:5173"}
	in.ASR.Backend = "whispercpp"
	in.Agent.Backend = "claude"
	in.Agent.Concurrency = 4
	in.Agent.Claude = map[string]string{"fact_pass": "sonnet", "synthesis": "opus"}
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
	if out.ASR.Backend != "whispercpp" {
		t.Errorf("asr backend = %q", out.ASR.Backend)
	}
	if out.Agent.Concurrency != 4 {
		t.Errorf("concurrency = %d", out.Agent.Concurrency)
	}
	if out.Agent.Claude["synthesis"] != "opus" {
		t.Errorf("claude synthesis = %q", out.Agent.Claude["synthesis"])
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
	t.Setenv("AUDIOSILO_SIDECARS_ASR_DEVICE", "cuda")
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
	if cfg.ASR.Device != "cuda" {
		t.Errorf("device = %q", cfg.ASR.Device)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]Config{
		"bad listen":       {Listen: "not-a-host-port", Agent: AgentConfig{Concurrency: 1}},
		"zero concurrency": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 0}},
		"origin with path": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"http://x.example/foo"}},
		"origin no scheme": {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"x.example"}},
		"origin ftp":       {Listen: DefaultListen, Agent: AgentConfig{Concurrency: 1}, CORSOrigins: []string{"ftp://x.example"}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}
}

func TestValidateAcceptsGoodOrigins(t *testing.T) {
	cfg := Default()
	cfg.CORSOrigins = []string{"http://localhost:5173", "https://ui.example.com"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
