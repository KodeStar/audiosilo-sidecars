package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// claudeBinName is the default binary name resolved on PATH when no explicit path
// is configured.
const claudeBinName = "claude"

// claudeRunner drives the Claude Code CLI in headless mode. It authenticates by the
// CLI's own login by default; when an anthropic key is present in secrets it is
// injected as ANTHROPIC_API_KEY into the child environment (never argv).
type claudeRunner struct {
	binResolver
	sec secrets.Store
}

func newClaudeRunner(explicit string, sec secrets.Store) *claudeRunner {
	return &claudeRunner{binResolver: binResolver{name: claudeBinName, explicit: explicit}, sec: sec}
}

func (c *claudeRunner) ID() string { return IDClaude }

// SupportsWeb is always true for claude: WebSearch/WebFetch are first-class tools
// enabled by adding them to --allowedTools.
func (c *claudeRunner) SupportsWeb() bool { return true }

// Detect resolves the binary and runs `claude --version` (via the shared resolver).
func (c *claudeRunner) Detect(ctx context.Context) Availability {
	return c.binResolver.Detect(ctx)
}

// claudeResult is the single JSON result envelope claude -p --output-format json
// prints on stdout. Parsed defensively: unknown fields are ignored.
type claudeResult struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"`
	IsError      bool        `json:"is_error"`
	Result       string      `json:"result"`
	NumTurns     int         `json:"num_turns"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	Usage        claudeUsage `json:"usage"`
}

type claudeUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// buildArgs assembles the argv (no prompt - that rides on stdin). --model is
// omitted when empty so the CLI default applies.
func (c *claudeRunner) buildArgs(req Request) []string {
	tools := baseAllowedTools
	if req.NoTools {
		tools = ""
	} else if req.Web {
		tools += "," + webTools
	}
	turns := req.MaxTurns
	if turns < 1 {
		turns = maxTurns
	}
	args := []string{
		"-p",
		"--output-format", "json",
		"--max-turns", fmt.Sprintf("%d", turns),
		// --tools is the actual availability boundary; --allowedTools only
		// pre-approves tools. Supplying both prevents permission prompts without
		// leaving unrelated coding tools in the model's tool catalogue.
		"--tools", tools,
		"--allowedTools", tools,
		// These invocations are disposable ETL jobs, not coding sessions. Do not
		// persist a resumable session. Deliberately avoid --bare: it disables keychain
		// reads, including the Claude subscription login this runner relies on.
		"--no-session-persistence",
		"--system-prompt", "You are a deterministic document-processing worker. Follow the stdin task exactly, use only the provided tools and paths, write the requested artifacts, and stop.",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

// Run executes one claude invocation and parses its result envelope. A timeout or
// parent cancel is returned as-is; is_error, a nonzero exit, or an unparseable
// envelope becomes an error carrying truncated detail; a rate-limit signature in
// the result or stderr becomes a *RateLimitError.
func (c *claudeRunner) Run(ctx context.Context, req Request) (Result, error) {
	p, err := c.resolve()
	if err != nil {
		return Result{}, &NotAvailableError{Backend: IDClaude, Detail: err.Error()}
	}

	key := ""
	if c.sec != nil {
		key, _ = c.sec.Get(secrets.AnthropicAPIKey)
	}
	env := childEnv(map[string]string{"ANTHROPIC_API_KEY": key})

	stdout, stderr, runErr := runCLI(ctx, cliSpec{
		path:      p,
		args:      c.buildArgs(req),
		dir:       req.Dir,
		env:       env,
		stdin:     req.Prompt,
		timeout:   req.Timeout,
		heartbeat: req.Heartbeat,
		process:   req.Process,
	})

	if errors.Is(runErr, errTimeout) {
		return Result{}, fmt.Errorf("claude timed out after %s", req.Timeout)
	}
	if runErr != nil && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	var res claudeResult
	parseErr := json.Unmarshal([]byte(stdout), &res)

	// Classify a rate-limit before anything else: it can arrive as is_error with a
	// rate-limit result, or as a nonzero exit with the message on stderr.
	detail := firstNonEmpty(res.Result, stderr, stdout)
	if isRateLimit(res.Result) || isRateLimit(stderr) {
		return Result{}, newRateLimitError(detail, time.Now())
	}

	if runErr != nil {
		return Result{}, fmt.Errorf("claude exited: %w: %s", runErr, truncate(detail))
	}
	if parseErr != nil {
		return Result{}, fmt.Errorf("claude: parse result json: %w: %s", parseErr, truncate(stdout))
	}
	if res.IsError {
		return Result{}, fmt.Errorf("claude reported error: %s", truncate(firstNonEmpty(res.Result, res.Subtype)))
	}

	return Result{
		Text: res.Result,
		Usage: Usage{
			Model:        req.Model,
			Input:        res.Usage.InputTokens,
			Output:       res.Usage.OutputTokens,
			CacheRead:    res.Usage.CacheReadInputTokens,
			CostUSD:      res.TotalCostUSD,
			CostReported: true,
			Turns:        res.NumTurns,
		},
	}, nil
}
