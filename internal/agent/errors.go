package agent

import "fmt"

// RateLimitError marks a backend failure the caller should retry after a backoff
// rather than fail. It is recognized from a backend's output signatures (claude
// result text/stderr, codex turn.failed text) and is errors.As-able.
type RateLimitError struct {
	Detail string
}

func (e *RateLimitError) Error() string {
	if e.Detail == "" {
		return "agent: rate limited"
	}
	return "agent: rate limited: " + e.Detail
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
