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

// DefaultConcurrency is the default number of parallel agent slots (Lane B),
// honoured by the scheduler's agent lane (M1).
const DefaultConcurrency = 2

// DefaultMetadataBaseURL is the community metadata API the coverage/lookup client
// queries. Overridable (env / config) so tests can point at a local httptest.
const DefaultMetadataBaseURL = "https://meta.audiosilo.app"

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
// whisper.cpp binary. The device is NOT a config knob (no backend honors an
// override yet) - /system reports the device the resolved backend actually
// detected; a device override can return here once a backend honors it.
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

// AgentConfig holds the agent-runner settings. Typed stub for M0; the runner that
// consumes it lands in a later milestone. Model routing is config, never
// hardcoded: Claude/OpenAI map a pipeline stage name to a model name.
type AgentConfig struct {
	Backend     string            `yaml:"backend"`     // "" | "claude" | "codex" (M5)
	Concurrency int               `yaml:"concurrency"` // parallel agent slots (M6)
	Claude      map[string]string `yaml:"claude"`      // stage -> model (M5)
	OpenAI      map[string]string `yaml:"openai"`      // stage -> model (M5)
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
	// $PATH turns up the tool. Set false for an air-gapped/managed environment.
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
		Agent:        AgentConfig{Concurrency: DefaultConcurrency},
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
