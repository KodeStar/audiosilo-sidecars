// Package agent runs a headless coding-agent CLI (claude or codex) as one step of
// the sidecars pipeline. It is the M5 seam that turns a staged context directory
// plus a rendered prompt into structured outputs on disk, capturing token/cost
// usage and translating the two vendors' invocation and output contracts behind a
// single Runner interface.
//
// Design invariants (see M5-DESIGN.md section 2):
//   - The prompt is delivered on STDIN, never argv (a large prompt on argv hit the
//     128KiB E2BIG limit in the meta ai-verify work).
//   - An injected API key is passed only through the child process environment; its
//     VALUE must never appear in argv, in a returned error, or in anything logged.
//   - A per-invocation timeout kills the whole child process group, not just the
//     immediate child (an agent CLI spawns helper subprocesses).
//   - This package never imports internal/config: model routing and path knobs
//     arrive as plain strings/maps so agent stays a leaf the config layer wires up.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// Backend IDs, also the config `agent.backend` values and the telemetry strings.
const (
	IDClaude = "claude"
	IDCodex  = "codex"
)

// maxTurns caps a single agent run's tool-use turns. A generous ceiling: the
// pipeline's own retry/validation loop is the real bound, this only stops a runaway
// session.
const maxTurns = 200

// baseAllowedTools is the file-only tool whitelist every stage gets. Web-enabled
// stages additionally get webTools. claudeRunner passes the list to both --tools
// (availability boundary) and --allowedTools (permission pre-approval).
const (
	baseAllowedTools = "Read,Write,Edit,Glob,Grep"
	webTools         = "WebSearch,WebFetch"
)

// Availability reports whether a backend can run and, if so, its resolved path and
// version. Detail carries a human message when Available is false ("claude CLI not
// found on PATH", "configured claude_path not found: ...").
type Availability struct {
	Backend   string
	Available bool
	Path      string
	Version   string
	Detail    string
}

// binResolver resolves an agent CLI binary by name, honoring an optional explicit
// path override, and performs the shared Detect (resolve + `<bin> --version`). Both
// runners embed it so the boilerplate lives in one place.
type binResolver struct {
	name     string // default binary name resolved on PATH
	explicit string // configured explicit path ("" = resolve name on PATH)
}

// resolve returns the binary path, or a loud error when an explicitly configured
// path does not resolve (a typo must not be silently ignored).
func (b binResolver) resolve() (string, error) {
	if b.explicit != "" {
		p, err := exec.LookPath(b.explicit)
		if err != nil {
			return "", fmt.Errorf("configured %s_path not found: %s", b.name, b.explicit)
		}
		return p, nil
	}
	p, err := exec.LookPath(b.name)
	if err != nil {
		return "", fmt.Errorf("%s CLI not found on PATH", b.name)
	}
	return p, nil
}

// Detect resolves the binary and runs `<bin> --version`. On success Path/Version/
// Available are set. On failure Detail explains, and Path stays "" ONLY when the
// binary could not be resolved at all - a --version failure still records the
// resolved Path. Select reads av.Path to tell an unresolved explicit path (the loud
// misconfiguration) from a resolved binary whose --version merely failed. The backend
// id reported is b.name (the runner backend IDs equal the binary names).
func (b binResolver) Detect(ctx context.Context) Availability {
	av := Availability{Backend: b.name}
	p, err := b.resolve()
	if err != nil {
		av.Detail = err.Error()
		return av
	}
	av.Path = p
	ver, verr := runVersion(ctx, p, "--version")
	if verr != nil {
		av.Detail = b.name + " --version failed: " + truncate(verr.Error())
		return av
	}
	av.Available = true
	av.Version = ver
	return av
}

// Request is one agent invocation.
type Request struct {
	Stage   string        // pipeline stage name; telemetry only
	Dir     string        // staged dir; becomes the child's cwd
	Prompt  string        // full prompt text, delivered on stdin
	Model   string        // "" = the backend CLI's default model
	Web     bool          // allow the web search/fetch tools
	Timeout time.Duration // per-invocation wall-clock cap; 0 = no timeout
	// MaxTurns overrides the backend's runaway ceiling when positive. Stages set a
	// bounded value appropriate to their file set; zero retains maxTurns for callers
	// outside the pipeline.
	MaxTurns int
	// NoTools is used by the orchestration supervisor: the model receives bounded
	// structured context and must not inspect or edit the workspace.
	NoTools bool
	// Process reports the actual child pid lifecycle for liveness reconciliation.
	Process func(pid int, active bool)
	// Heartbeat, when non-nil, is called periodically (every heartbeatInterval) WHILE
	// the CLI subprocess is genuinely running - between cmd.Start and cmd.Wait
	// returning - with the elapsed wall-time since the process started. It is a real
	// liveness signal: it does not fire during rate-limit backoff (that sleep happens
	// in RunWithBackoff, outside the subprocess) or before/after the process runs. The
	// pipeline uses it to emit an "agent still running" note on long agent stages.
	Heartbeat func(elapsed time.Duration)
}

// Usage is the token/cost accounting for one invocation. CostUSD is 0 when the
// backend does not report a dollar figure (codex).
type Usage struct {
	Model     string
	Input     int64 // input tokens, excluding cache reads
	Output    int64
	CacheRead int64
	CostUSD   float64
	// CostReported distinguishes a provider-reported zero from an unavailable cost.
	CostReported bool
	Turns        int
}

// Result is a successful agent run: the final assistant text plus usage. The real
// deliverables are the files the agent wrote under the staged out/ dir; Text is the
// summary the CLI returns.
type Result struct {
	Text  string
	Usage Usage
}

// Runner is one agent-CLI backend. Detect is cheap (resolve the binary + a fast
// --version). Run performs one invocation against a staged dir. SupportsWeb reports
// whether the backend can enable web search/fetch tools at all (so a stage can
// degrade gracefully when it cannot).
type Runner interface {
	ID() string
	Detect(ctx context.Context) Availability
	Run(ctx context.Context, req Request) (Result, error)
	SupportsWeb() bool
}

// SelectConfig is the minimal, config-package-free view Select needs: the chosen
// backend ("" = auto) and the optional explicit CLI paths. Kept deliberately small
// so internal/config can add fields without touching internal/agent.
type SelectConfig struct {
	Backend    string
	ClaudePath string
	CodexPath  string
}

// Select resolves the configured or auto-detected runner and reports its
// availability.
//
//   - backend "claude"/"codex": that runner, whether or not it is currently
//     available (an unavailable one still lets /system explain the state and lets
//     the pipeline park the book with an actionable message).
//   - backend "" or "auto": claude if detectable, else codex if detectable, else a
//     nil runner with Available=false.
//
// It returns a non-nil error only for two loud misconfigurations, mirroring the
// asr.whisper_cli_path policy: an unknown backend name, or an EXPLICITLY selected
// backend whose configured path does not resolve (a typo must not be silently
// swallowed). In auto mode an unresolved path is not fatal - Select just moves on
// to the next candidate.
func Select(ctx context.Context, cfg SelectConfig, sec secrets.Store) (Runner, Availability, error) {
	backend := strings.TrimSpace(cfg.Backend)
	switch backend {
	case IDClaude, IDCodex:
		r, explicit := runnerFor(backend, cfg, sec)
		av := r.Detect(ctx)
		// The loud misconfiguration: an explicitly selected path that does not
		// resolve. av.Path stays "" only when the binary could not be resolved at
		// all - a --version failure still records the resolved Path.
		if explicit != "" && !av.Available && av.Path == "" {
			return r, av, &NotAvailableError{Backend: backend, Detail: av.Detail}
		}
		return r, av, nil
	case "", "auto":
		c := newClaudeRunner(cfg.ClaudePath, sec)
		if av := c.Detect(ctx); av.Available {
			return c, av, nil
		}
		x := newCodexRunner(cfg.CodexPath, sec)
		if av := x.Detect(ctx); av.Available {
			return x, av, nil
		}
		return nil, Availability{Detail: "no agent CLI found; install the claude or codex CLI, or set agent.backend and the matching path"}, nil
	default:
		return nil, Availability{Backend: cfg.Backend, Detail: "unknown agent.backend"},
			&NotAvailableError{Backend: cfg.Backend, Detail: "unknown agent.backend (want \"\", \"claude\", or \"codex\")"}
	}
}

// runnerFor builds the runner for an explicitly selected backend and returns its
// configured explicit path (so Select can classify an unresolvable path as a loud
// misconfiguration). backend must be IDClaude or IDCodex.
func runnerFor(backend string, cfg SelectConfig, sec secrets.Store) (Runner, string) {
	if backend == IDCodex {
		return newCodexRunner(cfg.CodexPath, sec), cfg.CodexPath
	}
	return newClaudeRunner(cfg.ClaudePath, sec), cfg.ClaudePath
}

// ModelFor resolves the model name for a stage: the claude map for the claude
// backend, the openai map for codex. A missing key returns "" (the backend CLI's
// default model). Pure and config-free by design - the maps come from
// config.AgentConfig.Claude / .OpenAI upstream.
func ModelFor(claudeMap, openaiMap map[string]string, backendID, stage string) string {
	switch backendID {
	case IDClaude:
		return claudeMap[stage]
	case IDCodex:
		return openaiMap[stage]
	default:
		return ""
	}
}

// rateLimitSignatures are the case-insensitive markers that classify a backend
// failure as a rate-limit/overload (a retryable-after-backoff condition) rather
// than a hard error.
var rateLimitSignatures = []string{
	"rate limit",
	"rate_limit",
	"429",
	"overloaded",
	"usage limit",
}

// isRateLimit reports whether s contains any rate-limit signature (case-insensitive).
func isRateLimit(s string) bool {
	low := strings.ToLower(s)
	for _, sig := range rateLimitSignatures {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// maxDetail bounds how much CLI output rides in an error string, so a runaway CLI
// cannot flood logs or the events durable sink.
const maxDetail = 2000

// truncate trims s to at most maxDetail bytes, appending an elision marker.
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxDetail {
		return s
	}
	return s[:maxDetail] + "... (truncated)"
}

// firstNonEmpty returns the first non-blank string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
