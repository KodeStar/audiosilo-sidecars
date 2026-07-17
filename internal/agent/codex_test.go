package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

const codexOK = `{"type":"item.completed","item":{"type":"agent_message","text":"agent message text"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":50}}
{"type":"turn.completed","usage":{"input_tokens":30,"cached_input_tokens":5,"output_tokens":15}}`

func TestCodexRunSuccessAccumulatesUsage(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "codex 1.0", response: codexOK, lastMsg: "final answer"})
	r := newCodexRunner(path, secrets.NewMemStore())
	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "codex prompt", Model: "gpt-x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Final text comes from the --output-last-message file.
	if res.Text != "final answer" {
		t.Errorf("Text = %q, want %q", res.Text, "final answer")
	}
	// Codex usage is cumulative session totals, so the LAST turn.completed event wins
	// (not the sum). Turns still counts the two events.
	want := Usage{Model: "gpt-x", Input: 30, Output: 15, CacheRead: 5, CostUSD: 0, Turns: 2}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	if got := readCapture(t, capDir, "stdin.txt"); got != "codex prompt" {
		t.Errorf("prompt on stdin = %q", got)
	}
}

func TestCodexFallsBackToLastAgentMessage(t *testing.T) {
	// No lastMsg written to the output file -> fall back to the agent_message item.
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: codexOK})
	r := newCodexRunner(path, secrets.NewMemStore())
	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "agent message text" {
		t.Errorf("Text = %q, want fallback agent message", res.Text)
	}
}

func TestCodexTurnFailed(t *testing.T) {
	resp := `{"type":"turn.failed","error":"model produced an internal error"}`
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	r := newCodexRunner(path, secrets.NewMemStore())
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "codex turn failed") {
		t.Fatalf("want turn-failed error, got %v", err)
	}
}

func TestCodexRateLimit(t *testing.T) {
	resp := `{"type":"turn.failed","error":"429 rate_limit exceeded, slow down"}`
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	r := newCodexRunner(path, secrets.NewMemStore())
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitError, got %v", err)
	}
}

func TestCodexWebFlagAddsOverride(t *testing.T) {
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: codexOK, lastMsg: "x"})
	r := newCodexRunner(path, secrets.NewMemStore())

	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", Web: false}); err != nil {
		t.Fatalf("Run no-web: %v", err)
	}
	if argv := readCapture(t, capDir, "argv.txt"); strings.Contains(argv, "web_search") {
		t.Errorf("no-web run should not set web_search: %q", argv)
	}

	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p", Web: true}); err != nil {
		t.Fatalf("Run web: %v", err)
	}
	argv := readCapture(t, capDir, "argv.txt")
	if !strings.Contains(argv, `web_search="live"`) {
		t.Errorf("web run should set the web_search override: %q", argv)
	}
	if !r.SupportsWeb() {
		t.Error("codex SupportsWeb should be true (documented -c override)")
	}
}

func TestCodexSecretInjectedNotLeaked(t *testing.T) {
	const secret = "sk-openai-TESTSECRET"
	resp := `{"type":"turn.failed","error":"boom without the key"}`
	path, capDir := fakeCLI(t, fakeCLIOpts{versionLine: "v", response: resp})
	sec := secrets.NewMemStore()
	if err := sec.Set(secrets.OpenAIAPIKey, secret); err != nil {
		t.Fatal(err)
	}
	r := newCodexRunner(path, sec)
	_, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Prompt: "p"})
	if err == nil {
		t.Fatal("expected error")
	}
	// Allowed: injected into BOTH CODEX_API_KEY and OPENAI_API_KEY.
	if got := readCapture(t, capDir, "codex.txt"); got != secret {
		t.Errorf("CODEX_API_KEY = %q, want secret", got)
	}
	if got := readCapture(t, capDir, "openai.txt"); got != secret {
		t.Errorf("OPENAI_API_KEY = %q, want secret", got)
	}
	// Denied: not in argv, not in the error.
	if argv := readCapture(t, capDir, "argv.txt"); strings.Contains(argv, secret) {
		t.Errorf("secret leaked into argv")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("secret leaked into error: %v", err)
	}
}

func TestCodexDetect(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "codex 1.2.3"})
	r := newCodexRunner(path, secrets.NewMemStore())
	av := r.Detect(context.Background())
	if !av.Available || av.Version != "codex 1.2.3" {
		t.Fatalf("Detect = %+v", av)
	}
}
