package agent

import (
	"fmt"
	"testing"
	"time"
)

func TestParseResetTimeEpochSuffix(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// A plausible reset: 2h in the future (inside the (now, now+48h] trust window).
	reset := now.Add(2 * time.Hour)
	got, ok := ParseResetTime(fmt.Sprintf("usage limit reached|%d", reset.Unix()), now)
	if !ok {
		t.Fatal("want ok for the |<epoch> form")
	}
	if want := time.Unix(reset.Unix(), 0).UTC(); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseResetTimeEpochInPastRejected: a |<epoch> that resolves to a past instant (a stale
// message) is not trusted - a genuine reset is always in the future.
func TestParseResetTimeEpochInPastRejected(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	if _, ok := ParseResetTime(fmt.Sprintf("limit|%d", past.Unix()), now); ok {
		t.Error("a past epoch should not parse (stale value / request-id lookalike)")
	}
}

// TestParseResetTimeEpochTooFarFutureRejected: a |<epoch> more than 48h ahead is a request-id
// lookalike (or a nonsense value), not a rate-limit reset, so it is rejected.
func TestParseResetTimeEpochTooFarFutureRejected(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	far := now.Add(72 * time.Hour) // beyond maxResetHorizon
	if _, ok := ParseResetTime(fmt.Sprintf("limit|%d", far.Unix()), now); ok {
		t.Error("an epoch beyond 48h should not parse")
	}
}

func TestParseResetTimeEpochRejectsWrongWidth(t *testing.T) {
	now := time.Now()
	// 11 digits after the pipe must not half-match the 10-digit rule.
	if _, ok := ParseResetTime("limit|17197500000", now); ok {
		t.Error("an 11-digit run should not parse as a 10-digit epoch")
	}
	if _, ok := ParseResetTime("limit|123", now); ok {
		t.Error("a short number should not parse")
	}
}

func TestParseResetTimeClockNextOccurrence(t *testing.T) {
	loc := time.FixedZone("TST", 0)
	// It is 14:00; "resets at 3pm" is later today.
	now := time.Date(2026, 7, 1, 14, 0, 0, 0, loc)
	got, ok := ParseResetTime("your limit resets at 3pm", now)
	if !ok {
		t.Fatal("want ok")
	}
	want := time.Date(2026, 7, 1, 15, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResetTimeClockRollsToTomorrow(t *testing.T) {
	loc := time.FixedZone("TST", 0)
	// It is 16:00; "resets at 3pm" already passed, so the next occurrence is tomorrow.
	now := time.Date(2026, 7, 1, 16, 0, 0, 0, loc)
	got, ok := ParseResetTime("resets at 3pm", now)
	if !ok {
		t.Fatal("want ok")
	}
	want := time.Date(2026, 7, 2, 15, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResetTimeClockMinutesAndAMPMAndCase(t *testing.T) {
	loc := time.FixedZone("TST", 0)
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, loc)
	got, ok := ParseResetTime("Will RESET AT 11:30 AM", now)
	if !ok {
		t.Fatal("want ok")
	}
	want := time.Date(2026, 7, 1, 11, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// 12am -> 00:00 (rolls to tomorrow since it is 08:00), 12pm -> 12:00.
	if g, _ := ParseResetTime("resets at 12pm", now); !g.Equal(time.Date(2026, 7, 1, 12, 0, 0, 0, loc)) {
		t.Errorf("12pm = %v", g)
	}
	if g, _ := ParseResetTime("resets at 12am", now); !g.Equal(time.Date(2026, 7, 2, 0, 0, 0, 0, loc)) {
		t.Errorf("12am = %v", g)
	}
}

func TestParseResetTimeUnparseable(t *testing.T) {
	now := time.Now()
	for _, s := range []string{"", "rate limited, try again later", "resets at 25pm", "resets at 3", "resets soon"} {
		if _, ok := ParseResetTime(s, now); ok {
			t.Errorf("%q should not parse", s)
		}
	}
}

func TestNewRateLimitErrorParsesResetAt(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// A plausible near-future reset epoch is populated onto ResetAt.
	e := newRateLimitError(fmt.Sprintf("blocked|%d", now.Add(90*time.Minute).Unix()), now)
	if e.ResetAt.IsZero() {
		t.Fatal("ResetAt not populated from the detail")
	}
	if e2 := newRateLimitError("just rate limited", now); !e2.ResetAt.IsZero() {
		t.Error("ResetAt should be zero when the detail carries no reset hint")
	}
	// A stale/implausible epoch (far past) leaves ResetAt zero, so the caller uses its fallback.
	if e3 := newRateLimitError("blocked|1719750000", now); !e3.ResetAt.IsZero() {
		t.Error("ResetAt should be zero for an implausible (far-past) epoch")
	}
}
