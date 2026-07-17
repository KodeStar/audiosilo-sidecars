package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// codexBinName is the default binary name resolved on PATH when no explicit path is
// configured.
const codexBinName = "codex"

// codexRunner drives the OpenAI Codex CLI in non-interactive (`codex exec`) mode. It
// authenticates by the CLI's own ChatGPT login by default; when an openai key is
// present in secrets it is injected as CODEX_API_KEY and OPENAI_API_KEY into the
// child environment (never argv). Codex reports token usage but no dollar cost, so
// Usage.CostUSD stays 0.
type codexRunner struct {
	binResolver
	sec secrets.Store
}

func newCodexRunner(explicit string, sec secrets.Store) *codexRunner {
	return &codexRunner{binResolver: binResolver{name: codexBinName, explicit: explicit}, sec: sec}
}

func (r *codexRunner) ID() string { return IDCodex }

// SupportsWeb is true: codex exec enables live web search via the documented config
// override `-c web_search="live"` (the current top-level `web_search` key, string
// modes disabled|cached|indexed|live). Verified against the Codex config reference
// at https://learn.chatgpt.com/docs/config-file/config-reference (2026-07-16).
func (r *codexRunner) SupportsWeb() bool { return true }

// Detect resolves the binary and runs `codex --version` (via the shared resolver).
func (r *codexRunner) Detect(ctx context.Context) Availability {
	return r.binResolver.Detect(ctx)
}

// codexEvent is one line of the JSONL event stream. Parsed defensively; unknown
// fields and event types are ignored.
type codexEvent struct {
	Type  string      `json:"type"`
	Usage *codexUsage `json:"usage"`
	Item  *codexItem  `json:"item"`
	Error string      `json:"error"`
}

type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

type codexItem struct {
	Type string `json:"type"` // "agent_message" carries assistant text
	Text string `json:"text"`
}

// buildArgs assembles the codex argv. --model is omitted when empty (CLI default).
// lastMsgFile receives the final assistant message. Web search is enabled via the
// documented `-c web_search="live"` override.
func (r *codexRunner) buildArgs(req Request, lastMsgFile string) []string {
	args := []string{
		"exec",
		"--json",
		"--cd", req.Dir,
		"--sandbox", "workspace-write",
		"--skip-git-repo-check",
		"--output-last-message", lastMsgFile,
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Web {
		args = append(args, "-c", `web_search="live"`)
	}
	return args
}

// Run executes one codex invocation, reads usage from turn.completed events (last
// event wins - see parseCodexStream), and takes the final text from the
// --output-last-message file (falling back to the last agent_message item). A
// turn.failed event or nonzero exit becomes an error; a rate-limit signature becomes
// a *RateLimitError.
func (r *codexRunner) Run(ctx context.Context, req Request) (Result, error) {
	p, err := r.resolve()
	if err != nil {
		return Result{}, &NotAvailableError{Backend: IDCodex, Detail: err.Error()}
	}

	lastMsg, err := os.CreateTemp("", "codex-last-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("codex: temp file: %w", err)
	}
	lastMsgPath := lastMsg.Name()
	_ = lastMsg.Close()
	defer func() { _ = os.Remove(lastMsgPath) }()

	key := ""
	if r.sec != nil {
		key, _ = r.sec.Get(secrets.OpenAIAPIKey)
	}
	env := childEnv(map[string]string{"CODEX_API_KEY": key, "OPENAI_API_KEY": key})

	stdout, stderr, runErr := runCLI(ctx, cliSpec{
		path:      p,
		args:      r.buildArgs(req, lastMsgPath),
		dir:       req.Dir,
		env:       env,
		stdin:     req.Prompt,
		timeout:   req.Timeout,
		heartbeat: req.Heartbeat,
	})

	if errors.Is(runErr, errTimeout) {
		return Result{}, fmt.Errorf("codex timed out after %s", req.Timeout)
	}
	if runErr != nil && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	usage, lastAgentMsg, failDetail := parseCodexStream(stdout)

	if failDetail != "" {
		if isRateLimit(failDetail) {
			return Result{}, &RateLimitError{Detail: truncate(failDetail)}
		}
		return Result{}, fmt.Errorf("codex turn failed: %s", truncate(failDetail))
	}
	if isRateLimit(stderr) {
		return Result{}, &RateLimitError{Detail: truncate(firstNonEmpty(stderr, stdout))}
	}
	if runErr != nil {
		return Result{}, fmt.Errorf("codex exited: %w: %s", runErr, truncate(firstNonEmpty(stderr, stdout)))
	}

	text := lastAgentMsg
	if fileText, rerr := os.ReadFile(lastMsgPath); rerr == nil && strings.TrimSpace(string(fileText)) != "" { //nolint:gosec // lastMsgPath is our own os.CreateTemp file
		text = strings.TrimSpace(string(fileText))
	}

	usage.Model = req.Model
	return Result{Text: text, Usage: usage}, nil
}

// parseCodexStream reads the JSONL events, taking token usage from turn.completed
// events and tracking the last agent_message text and any turn.failed detail.
// Malformed lines are skipped.
//
// Token usage on codex turn.completed events is CUMULATIVE session totals, not
// per-turn deltas (openai/codex exec's event processor emits ThreadTokenUsage.total,
// never .last - see openai/codex issue #17539), so each event REPLACES the running
// Input/Output/CacheRead rather than adding to them: last event wins. Turns still
// increments per event to count the invocation's turns.
func parseCodexStream(stdout string) (usage Usage, lastAgentMsg, failDetail string) {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "turn.completed":
			if ev.Usage != nil {
				usage.Input = ev.Usage.InputTokens
				usage.Output = ev.Usage.OutputTokens
				usage.CacheRead = ev.Usage.CachedInputTokens
				usage.Turns++
			}
		case "turn.failed":
			failDetail = firstNonEmpty(ev.Error, failDetail, "codex reported a failed turn")
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
				lastAgentMsg = strings.TrimSpace(ev.Item.Text)
			}
		}
	}
	return usage, lastAgentMsg, failDetail
}
