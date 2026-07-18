package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

const claudeOK = `{"type":"result","subtype":"success","is_error":false,"result":"all done","num_turns":3,"total_cost_usd":0.0123,"usage":{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}`

func TestClaudeRunSuccessParsesUsage(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "claude 2.1.0", response: claudeOK})
	r := newClaudeRunner(path, secrets.NewMemStore())
	res, err := r.Run(context.Background(), Request{Stage: "fact_pass", Dir: t.TempDir(), Prompt: "do the thing", Model: "sonnet"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "all done" {
		t.Errorf("Text = %q", res.Text)
	}
	want := Usage{Model: "sonnet", Input: 200, Output: 80, CacheRead: 10, CostUSD: 0.0123, Turns: 3}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	if got := readCapture(t, capDir, "stdin.txt"); got != "do the thing" {
		t.Errorf("prompt on stdin = %q, want %q", got, "do the thing")
	}
}

func TestClaudeStdinNotArgv(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: claudeOK})
	r := newClaudeRunner(path, secrets.NewMemStore())
	prompt := "PROMPT-MARKER-should-not-be-in-argv"
	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: prompt}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if argv := readCapture(t, capDir, "argv.txt"); strings.Contains(argv, prompt) {
		t.Errorf("prompt leaked into argv: %q", argv)
	}
	if stdin := readCapture(t, capDir, "stdin.txt"); stdin != prompt {
		t.Errorf("stdin = %q, want %q", stdin, prompt)
	}
}

func TestClaudeWebFlagTogglesTools(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: claudeOK})
	r := newClaudeRunner(path, secrets.NewMemStore())

	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", Web: false}); err != nil {
		t.Fatalf("Run no-web: %v", err)
	}
	argv := readCapture(t, capDir, "argv.txt")
	if !strings.Contains(argv, baseAllowedTools) {
		t.Errorf("argv missing base tools: %q", argv)
	}
	if strings.Contains(argv, "WebSearch") {
		t.Errorf("no-web run should not enable WebSearch: %q", argv)
	}
	for _, flag := range []string{"--tools", "--allowedTools", "--no-session-persistence", "--system-prompt"} {
		if !strings.Contains(argv, flag) {
			t.Errorf("optimized invocation missing %s: %q", flag, argv)
		}
	}
	if strings.Contains(argv, "--bare") {
		t.Errorf("--bare disables keychain reads and breaks subscription authentication: %q", argv)
	}

	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", Web: true}); err != nil {
		t.Fatalf("Run web: %v", err)
	}
	argv = readCapture(t, capDir, "argv.txt")
	if !strings.Contains(argv, "WebSearch,WebFetch") {
		t.Errorf("web run should enable WebSearch,WebFetch: %q", argv)
	}
}

func TestClaudeMaxTurnsOverride(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: claudeOK})
	r := newClaudeRunner(path, secrets.NewMemStore())
	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", MaxTurns: 32}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv := readCapture(t, capDir, "argv.txt")
	if !strings.Contains(argv, "--max-turns 32") {
		t.Errorf("argv missing max-turns override: %q", argv)
	}
}

func TestClaudeIsErrorEnvelope(t *testing.T) {
	resp := `{"type":"result","is_error":true,"result":"boom"}`
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	r := newClaudeRunner(path, secrets.NewMemStore())
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want is_error surfaced, got %v", err)
	}
}

func TestClaudeNonZeroExit(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: "", exit: 2})
	r := newClaudeRunner(path, secrets.NewMemStore())
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "claude exited") {
		t.Fatalf("want exit error, got %v", err)
	}
}

func TestClaudeRateLimit(t *testing.T) {
	resp := `{"type":"result","is_error":true,"result":"Error: rate limit reached, please retry"}`
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	r := newClaudeRunner(path, secrets.NewMemStore())
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitError, got %v", err)
	}
}

func TestClaudeTimeoutKillsProcess(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: claudeOK, sleepSecs: 30})
	r := newClaudeRunner(path, secrets.NewMemStore())
	start := time.Now()
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", Timeout: 250 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout did not fire promptly: %s", elapsed)
	}
}

func TestClaudeSecretInjectedNotLeaked(t *testing.T) {
	const secret = "sk-ant-TESTSECRET-value"
	// A run that ERRORS while the key is set: proves the key reaches the child env
	// (allowed) but never the argv or the returned error (denied).
	resp := `{"type":"result","is_error":true,"result":"boom without the key"}`
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	sec := secrets.NewMemStore()
	if err := sec.Set(secrets.AnthropicAPIKey, secret); err != nil {
		t.Fatal(err)
	}
	r := newClaudeRunner(path, sec)
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err == nil {
		t.Fatal("expected error from is_error envelope")
	}
	// Allowed: the child saw the key in its environment.
	if got := readCapture(t, capDir, "anthropic.txt"); got != secret {
		t.Errorf("child ANTHROPIC_API_KEY = %q, want the secret", got)
	}
	// Denied: the key is not in argv and not in the error string.
	if argv := readCapture(t, capDir, "argv.txt"); strings.Contains(argv, secret) {
		t.Errorf("secret leaked into argv")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("secret leaked into error: %v", err)
	}
}

func TestClaudeNoSecretMeansEmptyEnv(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: claudeOK})
	r := newClaudeRunner(path, secrets.NewMemStore()) // no key set
	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readCapture(t, capDir, "anthropic.txt"); got != "" {
		t.Errorf("expected no injected key, child saw %q", got)
	}
}

func TestClaudeDetect(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "claude 2.1.211"})
	r := newClaudeRunner(path, secrets.NewMemStore())
	av := r.Detect(context.Background())
	if !av.Available || av.Version != "claude 2.1.211" || av.Path != path {
		t.Fatalf("Detect = %+v", av)
	}
	if r.SupportsWeb() != true {
		t.Error("claude SupportsWeb should be true")
	}
}

func TestClaudeDetectUnresolvedExplicit(t *testing.T) {
	r := newClaudeRunner("/no/such/claude/binary", secrets.NewMemStore())
	av := r.Detect(context.Background())
	if av.Available {
		t.Fatal("unresolved explicit path should be unavailable")
	}
	if !strings.Contains(av.Detail, "configured claude_path not found") {
		t.Errorf("Detail = %q", av.Detail)
	}
	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"}); err == nil {
		t.Fatal("Run should fail when the binary is unresolved")
	} else {
		var na *NotAvailableError
		if !errors.As(err, &na) {
			t.Errorf("want *NotAvailableError, got %v", err)
		}
	}
}
