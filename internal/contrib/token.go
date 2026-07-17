package contrib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// ErrNoCredential is returned by TokenSource.Resolve when neither a GitHub PAT
// in the secrets store nor a `gh auth token` fallback yields a token. It is a
// sentinel so callers can errors.Is it (issue/pr contribution modes park the
// book on this; unauthenticated public polling ignores it).
var ErrNoCredential = errors.New("contrib: no GitHub credential (no PAT in secrets, and gh auth token is unavailable)")

// from values reported alongside a resolved token.
const (
	FromPAT = "pat"
	FromGH  = "gh"
)

// ghTokenTimeout bounds the `gh auth token` shell-out.
const ghTokenTimeout = 15 * time.Second

// TokenSource resolves a GitHub credential for the REST client. It prefers a
// PAT stored in the secrets keychain; failing that it shells out to the GitHub
// CLI (`gh auth token`). The resolved token is never logged or placed in argv.
type TokenSource struct {
	sec secrets.Store
	// ghToken resolves a token via the GitHub CLI. It is a field so tests can
	// substitute a fake; the default runs the real `gh auth token` shell-out.
	ghToken func(ctx context.Context) (string, error)
}

// NewTokenSource returns a TokenSource backed by sec (which may be nil, in
// which case only the gh fallback is consulted).
func NewTokenSource(sec secrets.Store) *TokenSource {
	ts := &TokenSource{sec: sec}
	ts.ghToken = func(ctx context.Context) (string, error) {
		ghPath, err := exec.LookPath("gh")
		if err != nil {
			return "", fmt.Errorf("gh CLI not found on PATH: %w", err)
		}
		return ghAuthTokenExec(ctx, ghPath)
	}
	return ts
}

// Resolve returns (token, from, nil) where from is FromPAT or FromGH. It tries
// the secrets PAT first, then the gh fallback. When neither yields a non-empty
// token it returns ("", "", ErrNoCredential). The error path never carries the
// token.
func (t *TokenSource) Resolve(ctx context.Context) (token, from string, err error) {
	if t.sec != nil {
		if pat, gerr := t.sec.Get(secrets.GitHubPAT); gerr == nil && pat != "" {
			return pat, FromPAT, nil
		}
	}
	if t.ghToken != nil {
		if tok, gerr := t.ghToken(ctx); gerr == nil && tok != "" {
			return tok, FromGH, nil
		}
	}
	return "", "", ErrNoCredential
}

// ghAuthTokenExec runs `<ghPath> auth token` (argv-only, no shell) with a short
// timeout and returns the trimmed stdout. On any failure it returns an error
// that NEVER includes stdout: `gh auth token` prints the token to stdout, so
// leaking stdout into an error would defeat the whole point. stderr (which gh
// does not use for the token) is included, trimmed, for debuggability.
func ghAuthTokenExec(ctx context.Context, ghPath string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()

	// argv-only exec (no shell), so there is no injection surface. The token
	// rides on stdout; we never place it in argv or an error.
	cmd := exec.CommandContext(runCtx, ghPath, "auth", "token")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if runErr := cmd.Run(); runErr != nil {
		if runCtx.Err() != nil && ctx.Err() == nil {
			return "", fmt.Errorf("gh auth token timed out after %s", ghTokenTimeout)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if msg := trimStderr(errBuf.String()); msg != "" {
			return "", fmt.Errorf("gh auth token failed: %s", msg)
		}
		// runErr is an *exec.ExitError whose Error() is "exit status N" - it does
		// not carry the child's output, so it cannot leak the token.
		return "", fmt.Errorf("gh auth token failed: %w", runErr)
	}
	return strings.TrimSpace(out.String()), nil
}

// trimStderr trims and caps a stderr snippet for inclusion in an error. gh never
// writes the token to stderr, so this is safe; the cap keeps errors small.
func trimStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
