package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

func TestSelectExplicitClaude(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "claude 2"})
	r, av, err := Select(context.Background(), SelectConfig{Backend: "claude", ClaudePath: path}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if r == nil || r.ID() != IDClaude || !av.Available {
		t.Fatalf("runner=%v av=%+v", r, av)
	}
}

func TestSelectExplicitCodex(t *testing.T) {
	path, _ := fakeCLI(t, fakeCLIOpts{versionLine: "codex 1"})
	r, av, err := Select(context.Background(), SelectConfig{Backend: "codex", CodexPath: path}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if r == nil || r.ID() != IDCodex || !av.Available {
		t.Fatalf("runner=%v av=%+v", r, av)
	}
}

func TestSelectExplicitPathMissingIsLoudError(t *testing.T) {
	_, _, err := Select(context.Background(), SelectConfig{Backend: "claude", ClaudePath: "/no/such/bin"}, secrets.NewMemStore())
	var na *NotAvailableError
	if !errors.As(err, &na) {
		t.Fatalf("want *NotAvailableError, got %v", err)
	}
}

func TestSelectUnknownBackend(t *testing.T) {
	_, _, err := Select(context.Background(), SelectConfig{Backend: "gemini"}, secrets.NewMemStore())
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("want unknown-backend error, got %v", err)
	}
}

func TestSelectAutoPrefersClaudeThenCodex(t *testing.T) {
	claudePath, _ := fakeCLI(t, fakeCLIOpts{versionLine: "claude"})
	codexPath, _ := fakeCLI(t, fakeCLIOpts{versionLine: "codex"})

	// Both available -> claude wins.
	r, _, err := Select(context.Background(), SelectConfig{Backend: "auto", ClaudePath: claudePath, CodexPath: codexPath}, secrets.NewMemStore())
	if err != nil || r == nil || r.ID() != IDClaude {
		t.Fatalf("auto both: runner=%v err=%v", r, err)
	}

	// Claude unresolved -> falls to codex.
	r, _, err = Select(context.Background(), SelectConfig{Backend: "auto", ClaudePath: "/no/claude", CodexPath: codexPath}, secrets.NewMemStore())
	if err != nil || r == nil || r.ID() != IDCodex {
		t.Fatalf("auto codex-fallback: runner=%v err=%v", r, err)
	}
}

func TestSelectAutoNoneAvailable(t *testing.T) {
	r, av, err := Select(context.Background(), SelectConfig{Backend: "auto", ClaudePath: "/no/claude", CodexPath: "/no/codex"}, secrets.NewMemStore())
	if err != nil {
		t.Fatalf("auto-none should not error: %v", err)
	}
	if r != nil {
		t.Errorf("expected nil runner, got %v", r)
	}
	if av.Available {
		t.Error("expected Available=false")
	}
}

func TestModelFor(t *testing.T) {
	claude := map[string]string{"fact_pass": "sonnet", "synthesizing": "opus"}
	openai := map[string]string{"fact_pass": "gpt-5"}
	cases := []struct {
		backend, stage, want string
	}{
		{"claude", "fact_pass", "sonnet"},
		{"claude", "synthesizing", "opus"},
		{"claude", "auditing", ""}, // missing key -> default
		{"codex", "fact_pass", "gpt-5"},
		{"codex", "synthesizing", ""},
		{"unknown", "fact_pass", ""},
	}
	for _, c := range cases {
		if got := ModelFor(claude, openai, c.backend, c.stage); got != c.want {
			t.Errorf("ModelFor(%s,%s) = %q, want %q", c.backend, c.stage, got, c.want)
		}
	}
}

func TestIsRateLimit(t *testing.T) {
	hits := []string{"Rate Limit exceeded", "error rate_limit", "HTTP 429", "model Overloaded", "monthly usage limit reached"}
	for _, s := range hits {
		if !isRateLimit(s) {
			t.Errorf("isRateLimit(%q) = false, want true", s)
		}
	}
	misses := []string{"all good", "validation failed", "not found"}
	for _, s := range misses {
		if isRateLimit(s) {
			t.Errorf("isRateLimit(%q) = true, want false", s)
		}
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("a", maxDetail+500)
	got := truncate(long)
	if len(got) <= maxDetail || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("truncate did not cap/mark: len=%d", len(got))
	}
	short := "brief"
	if truncate(short) != "brief" {
		t.Errorf("short string altered: %q", truncate(short))
	}
}
