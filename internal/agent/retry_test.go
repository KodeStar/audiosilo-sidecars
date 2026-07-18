package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptRunner returns a scripted sequence of (Result, error) pairs and records the
// prompts it was called with, so retry policy can be tested without a real CLI.
type scriptRunner struct {
	steps   []step
	calls   int
	prompts []string
}

type step struct {
	res Result
	err error
}

func (s *scriptRunner) ID() string                          { return "fake" }
func (s *scriptRunner) Detect(context.Context) Availability { return Availability{Available: true} }
func (s *scriptRunner) SupportsWeb() bool                   { return false }
func (s *scriptRunner) Run(_ context.Context, req Request) (Result, error) {
	s.prompts = append(s.prompts, req.Prompt)
	i := s.calls
	s.calls++
	if i >= len(s.steps) {
		return Result{}, errors.New("scriptRunner: out of steps")
	}
	return s.steps[i].res, s.steps[i].err
}

// tinyBackoff avoids real sleeps in tests.
func tinyBackoff(n int) []time.Duration {
	b := make([]time.Duration, n)
	for i := range b {
		b[i] = time.Millisecond
	}
	return b
}

func alwaysValid(Result) error { return nil }

func TestRunWithRetryImmediateSuccess(t *testing.T) {
	r := &scriptRunner{steps: []step{{res: Result{Text: "ok", Usage: Usage{Input: 5}}}}}
	var usages []Usage
	res, slept, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, alwaysValid, func(u Usage) { usages = append(usages, u) }, tinyBackoff(3))
	if err != nil || res.Text != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if slept != 0 {
		t.Errorf("slept = %v, want 0 (no rate-limit backoff)", slept)
	}
	if r.calls != 1 || len(usages) != 1 || usages[0].Input != 5 {
		t.Errorf("calls=%d usages=%v", r.calls, usages)
	}
}

func TestRunWithRetryValidationThenSuccess(t *testing.T) {
	r := &scriptRunner{steps: []step{{res: Result{Text: "bad"}}, {res: Result{Text: "good"}}}}
	firstDone := false
	validate := func(res Result) error {
		if !firstDone {
			firstDone = true
			return errors.New("missing field foo")
		}
		return nil
	}
	res, _, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, validate, func(Usage) {}, tinyBackoff(3))
	if err != nil || res.Text != "good" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if r.calls != 2 {
		t.Fatalf("calls=%d, want 2", r.calls)
	}
	got := r.prompts[1]
	if !strings.Contains(got, "failed validation") || !strings.Contains(got, "missing field foo") {
		t.Errorf("retry prompt did not append validator error: %q", got)
	}
	// The retry must re-issue the full base prompt (the fresh CLI process needs the
	// task context) ...
	if !strings.Contains(got, "base") {
		t.Errorf("retry prompt dropped the base prompt: %q", got)
	}
	// ... and instruct a PATCH of the prior output under out/ rather than a rebuild.
	if !strings.Contains(got, OutDirName+"/") {
		t.Errorf("retry prompt did not reference the out/ dir: %q", got)
	}
	if !strings.Contains(got, "Do not rebuild the outputs from scratch") {
		t.Errorf("retry prompt did not instruct patch-not-rebuild: %q", got)
	}
}

func TestRunWithRetryValidationExhausted(t *testing.T) {
	r := &scriptRunner{steps: []step{{}, {}, {}, {}}}
	validate := func(Result) error { return errors.New("still invalid") }
	_, _, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, validate, func(Usage) {}, tinyBackoff(3))
	if err == nil || !strings.Contains(err.Error(), "still invalid") {
		t.Fatalf("want validator error, got %v", err)
	}
	if r.calls != 3 { // 1 initial + 2 retries
		t.Errorf("calls=%d, want 3", r.calls)
	}
}

func TestRunWithRetryRateLimitBackoffThenSuccess(t *testing.T) {
	r := &scriptRunner{steps: []step{{err: &RateLimitError{Detail: "429"}}, {res: Result{Text: "ok"}}}}
	var usages int
	res, slept, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, alwaysValid, func(Usage) { usages++ }, tinyBackoff(3))
	if err != nil || res.Text != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if slept <= 0 {
		t.Errorf("slept = %v, want > 0 (one rate-limit backoff round elapsed)", slept)
	}
	if r.calls != 2 || usages != 2 {
		t.Errorf("calls=%d usages=%d (onUsage must fire on the rate-limited call too)", r.calls, usages)
	}
}

func TestRunWithRetryRateLimitExhausted(t *testing.T) {
	r := &scriptRunner{steps: []step{
		{err: &RateLimitError{Detail: "1"}},
		{err: &RateLimitError{Detail: "2"}},
		{err: &RateLimitError{Detail: "3"}},
		{err: &RateLimitError{Detail: "4"}},
	}}
	_, _, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, alwaysValid, func(Usage) {}, tinyBackoff(3))
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitError, got %v", err)
	}
	if r.calls != 4 { // 3 backoff rounds + the final rejected attempt
		t.Errorf("calls=%d, want 4", r.calls)
	}
}

func TestRunWithRetryRateLimitDoesNotConsumeOutputBudget(t *testing.T) {
	// A rate-limit, then three invalid outputs. If the rate-limit consumed an output
	// retry, only 2 invalid attempts would run; it must not, so all 3 do.
	r := &scriptRunner{steps: []step{
		{err: &RateLimitError{Detail: "rl"}},
		{res: Result{Text: "x"}},
		{res: Result{Text: "x"}},
		{res: Result{Text: "x"}},
	}}
	_, _, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, func(Result) error { return errors.New("invalid") }, func(Usage) {}, tinyBackoff(3))
	if err == nil {
		t.Fatal("want validator error")
	}
	if r.calls != 4 {
		t.Errorf("calls=%d, want 4 (1 rate-limit + 3 output attempts)", r.calls)
	}
}

func TestRunWithRetryOtherErrorFailsImmediately(t *testing.T) {
	r := &scriptRunner{steps: []step{{err: errors.New("disk full")}}}
	_, _, err := RunWithBackoff(context.Background(), r, Request{Prompt: "base"}, alwaysValid, func(Usage) {}, tinyBackoff(3))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want immediate failure, got %v", err)
	}
	if r.calls != 1 {
		t.Errorf("calls=%d, want 1", r.calls)
	}
}

func TestRunWithRetryContextCancelDuringBackoff(t *testing.T) {
	r := &scriptRunner{steps: []step{{err: &RateLimitError{Detail: "rl"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, _, err := RunWithBackoff(ctx, r, Request{Prompt: "base"}, alwaysValid, func(Usage) {}, []time.Duration{time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunWithRetryUsesDefaultBackoffLength(t *testing.T) {
	if got := len(DefaultBackoff()); got != 3 {
		t.Errorf("DefaultBackoff length = %d, want 3", got)
	}
}
