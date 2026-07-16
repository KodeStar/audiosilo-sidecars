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

// TestMigrateLegacyFFprobe proves a pre-Tools config's scan.ffprobe_path is
// adopted into the canonical tools.ffprobe_path on load.
func TestMigrateLegacyFFprobe(t *testing.T) {
	dir := t.TempDir()
	legacy := "metadata:\n  base_url: https://meta.audiosilo.app\nscan:\n  ffprobe_path: /legacy/ffprobe\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.FFprobePath != "/legacy/ffprobe" {
		t.Errorf("legacy scan.ffprobe_path not migrated: tools.ffprobe_path = %q", cfg.Tools.FFprobePath)
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
