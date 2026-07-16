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

	"gopkg.in/yaml.v3"
)

// FileName is the config file inside the data directory.
const FileName = "config.yaml"

// DefaultListen is the default bind address: loopback only, so the daemon is not
// exposed to the network unless the operator opts in.
const DefaultListen = "127.0.0.1:8090"

// DefaultConcurrency is the default number of parallel agent slots (Lane B). It
// is a stub for M0 - the scheduler that honours it lands in a later milestone.
const DefaultConcurrency = 2

// ASRConfig holds automatic-speech-recognition settings. It is a typed stub for
// M0: the fields are wired through so the Settings UI and later milestones share
// one shape, but no ASR backend runs yet.
type ASRConfig struct {
	Backend string `yaml:"backend"` // "" | "mlxwhisper" | "whispercpp" (M3)
	Device  string `yaml:"device"`  // "" | "metal" | "cuda" | "vulkan" | "cpu" (M3)
	Model   string `yaml:"model"`   // "" defaults to large-v3-turbo (M3)
}

// AgentConfig holds the agent-runner settings. Typed stub for M0; the runner that
// consumes it lands in a later milestone. Model routing is config, never
// hardcoded: Claude/OpenAI map a pipeline stage name to a model name.
type AgentConfig struct {
	Backend     string            `yaml:"backend"`     // "" | "claude" | "codex" (M5)
	Concurrency int               `yaml:"concurrency"` // parallel agent slots (M6)
	Claude      map[string]string `yaml:"claude"`      // stage -> model (M5)
	OpenAI      map[string]string `yaml:"openai"`      // stage -> model (M5)
}

// Config is the daemon configuration.
type Config struct {
	// Listen is the HTTP bind address (host:port). Defaults to loopback.
	Listen string `yaml:"listen"`
	// CORSOrigins is the allow-list of browser origins permitted to call the API
	// cross-origin (for a separately-deployed UI container). Empty = same-origin
	// only, which is the secure default.
	CORSOrigins []string `yaml:"cors_origins"`
	// ASR and Agent are typed stubs consumed by later milestones.
	ASR   ASRConfig   `yaml:"asr"`
	Agent AgentConfig `yaml:"agent"`
}

// Default returns a Config with secure defaults.
func Default() Config {
	return Config{
		Listen:      DefaultListen,
		CORSOrigins: []string{},
		ASR:         ASRConfig{},
		Agent:       AgentConfig{Concurrency: DefaultConcurrency},
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
	if cfg.CORSOrigins == nil {
		cfg.CORSOrigins = []string{}
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
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_BACKEND"); ok {
		cfg.ASR.Backend = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_ASR_DEVICE"); ok {
		cfg.ASR.Device = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_BACKEND"); ok {
		cfg.Agent.Backend = v
	}
	if v, ok := os.LookupEnv("AUDIOSILO_SIDECARS_AGENT_CONCURRENCY"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Agent.Concurrency = n
		}
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

// Validate reports whether the configuration is internally consistent.
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("invalid listen address %q: %w", c.Listen, err)
	}
	if c.Agent.Concurrency < 1 {
		return fmt.Errorf("agent.concurrency must be >= 1, got %d", c.Agent.Concurrency)
	}
	for _, o := range c.CORSOrigins {
		if err := validateOrigin(o); err != nil {
			return err
		}
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
