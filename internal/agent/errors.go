package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RateLimitError marks a backend failure the caller should retry after a backoff
// rather than fail. It is recognized from a backend's output signatures (claude
// result text/stderr, codex turn.failed text) and is errors.As-able.
//
// ResetAt is the best-effort parsed reset instant from the backend's message (see
// ParseResetTime), zero when the message carried none. The pipeline uses it to schedule
// an automatic re-admit shortly after the limit window clears, instead of a blind
// fixed wait.
type RateLimitError struct {
	Detail  string
	ResetAt time.Time
}

func (e *RateLimitError) Error() string {
	if e.Detail == "" {
		return "agent: rate limited"
	}
	return "agent: rate limited: " + e.Detail
}

// newRateLimitError builds a *RateLimitError from a backend's raw detail: it truncates
// the detail for storage but parses the reset instant from the FULL text first (the
// epoch/clock hint can sit past the truncation point). now is the reference for the
// "next occurrence" clock form; callers pass time.Now().
func newRateLimitError(detail string, now time.Time) *RateLimitError {
	e := &RateLimitError{Detail: truncate(detail)}
	if t, ok := ParseResetTime(detail, now); ok {
		e.ResetAt = t
	}
	return e
}

// resetEpochRe matches a "|<10-digit unix epoch>" suffix some Claude limit messages
// carry (e.g. "...rate limit|1719750000"). Exactly 10 digits (year 2001-2286) bounded by
// a non-digit or end, so an 11-digit run does not half-match.
var resetEpochRe = regexp.MustCompile(`\|(\d{10})(?:\D|$)`)

// resetClockRe matches "reset(s) at <H>[:<MM>] (am|pm)" (case-insensitive), the other
// form Claude uses (e.g. "resets at 3pm", "will reset at 11:30 AM").
//
// The wall-clock time carries no timezone, so ParseResetTime DELIBERATELY interprets it in
// the host's LOCAL location (now.Location()). That can be wrong if the CLI reports a time in
// a different zone, but the harm is bounded: rateLimitRetryAt floors every rate-limit readmit
// at now+rateLimitMinDelay, so even a badly-skewed reset can only make the book wait longer
// (never retry in the past), and the readmit is a cheap re-check, not a spend.
var resetClockRe = regexp.MustCompile(`(?i)resets?\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)

// maxResetHorizon bounds how far in the future a parsed |<epoch> reset instant may sit and
// still be trusted. A bare 10-digit run is also what a request id / trace token looks like,
// and a stale message can carry an old epoch - a genuine rate-limit reset is always in the
// near future, so anything outside (now, now+48h] is rejected as a lookalike/stale value.
const maxResetHorizon = 48 * time.Hour

// ParseResetTime extracts a rate-limit reset instant from a backend's message, relative
// to now. It recognizes two forms:
//
//   - a "|<10-digit unix epoch>" suffix -> that absolute instant (UTC);
//   - "reset(s) at <H>[:<MM>] (am|pm)" (case-insensitive) -> the NEXT occurrence of that
//     wall-clock time after now, interpreted in now's LOCAL location (so "resets at 3pm"
//     when it is already past 3pm today means 3pm tomorrow).
//
// Anything unparseable (or an out-of-range hour/minute) returns ok=false. Best-effort:
// the caller falls back to a fixed delay when this yields nothing.
func ParseResetTime(detail string, now time.Time) (time.Time, bool) {
	if m := resetEpochRe.FindStringSubmatch(detail); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			// Plausibility gate: only trust the epoch when it lands in the near future
			// (after now, within maxResetHorizon) - a real reset always does. An
			// implausible value (in the past, or absurdly far ahead) is a request-id
			// lookalike or a stale hint, so fall through to the clock form (there is no
			// second epoch form to try) and ultimately ok=false.
			if t := time.Unix(n, 0).UTC(); t.After(now) && t.Sub(now) <= maxResetHorizon {
				return t, true
			}
		}
	}
	if m := resetClockRe.FindStringSubmatch(detail); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		if hour < 1 || hour > 12 || minute > 59 {
			return time.Time{}, false
		}
		h := hour % 12 // 12am -> 0, 12pm handled by the +12 below
		if strings.EqualFold(m[3], "pm") {
			h += 12
		}
		cand := time.Date(now.Year(), now.Month(), now.Day(), h, minute, 0, 0, now.Location())
		if !cand.After(now) {
			cand = cand.Add(24 * time.Hour) // already past today -> the next occurrence
		}
		return cand, true
	}
	return time.Time{}, false
}

// NotAvailableError marks a backend that cannot run (binary unresolved, an explicit
// configured path missing, or an unknown backend name). errors.As-able so a stage
// can Park with an actionable message instead of failing hard.
type NotAvailableError struct {
	Backend string
	Detail  string
}

func (e *NotAvailableError) Error() string {
	if e.Backend == "" {
		return "agent backend unavailable: " + e.Detail
	}
	return fmt.Sprintf("agent backend %q unavailable: %s", e.Backend, e.Detail)
}
