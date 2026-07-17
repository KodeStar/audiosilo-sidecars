package contrib

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
)

// TestResolvePAT: a PAT in the secrets store wins and reports "pat"; the gh
// fallback is never consulted.
func TestResolvePAT(t *testing.T) {
	sec := secrets.NewMemStore()
	if err := sec.Set(secrets.GitHubPAT, "ghp_secretpat"); err != nil {
		t.Fatal(err)
	}
	ts := NewTokenSource(sec)
	ts.ghToken = func(context.Context) (string, error) {
		t.Fatal("gh fallback must not be consulted when a PAT is present")
		return "", nil
	}
	tok, from, err := ts.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "ghp_secretpat" || from != FromPAT {
		t.Fatalf("got (%q, %q), want (ghp_secretpat, pat)", tok, from)
	}
}

// TestResolveGHFallback: no PAT, but the gh CLI yields a token -> "gh".
func TestResolveGHFallback(t *testing.T) {
	ts := NewTokenSource(secrets.NewMemStore())
	ts.ghToken = func(context.Context) (string, error) { return "gho_fromcli", nil }
	tok, from, err := ts.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != "gho_fromcli" || from != FromGH {
		t.Fatalf("got (%q, %q), want (gho_fromcli, gh)", tok, from)
	}
}

// TestResolveNoCredential: no PAT and a failing gh fallback -> ErrNoCredential.
func TestResolveNoCredential(t *testing.T) {
	ts := NewTokenSource(secrets.NewMemStore())
	ts.ghToken = func(context.Context) (string, error) { return "", errors.New("gh not logged in") }
	_, _, err := ts.Resolve(context.Background())
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}

	// An empty token (no error) is also "no credential".
	ts.ghToken = func(context.Context) (string, error) { return "", nil }
	if _, _, err := ts.Resolve(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty-token err = %v, want ErrNoCredential", err)
	}
}

// TestResolveNilSecrets: a nil secrets store falls straight to the gh fallback.
func TestResolveNilSecrets(t *testing.T) {
	ts := NewTokenSource(nil)
	ts.ghToken = func(context.Context) (string, error) { return "gho_x", nil }
	tok, from, err := ts.Resolve(context.Background())
	if err != nil || tok != "gho_x" || from != FromGH {
		t.Fatalf("got (%q, %q, %v)", tok, from, err)
	}
}

// TestGhAuthTokenExecReal exercises the real argv-only shell-out against a fake
// gh script that prints a token to stdout.
func TestGhAuthTokenExecReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script fixture is POSIX")
	}
	gh := writeFakeGh(t, "#!/bin/sh\necho gho_realtoken\n")
	tok, err := ghAuthTokenExec(context.Background(), gh)
	if err != nil {
		t.Fatalf("ghAuthTokenExec: %v", err)
	}
	if tok != "gho_realtoken" {
		t.Fatalf("token = %q, want gho_realtoken (trimmed)", tok)
	}
}

// TestGhAuthTokenExecNeverLeaksToken is the security regression: a gh that
// prints the token to stdout but exits non-zero must NOT surface the token in
// the returned error.
func TestGhAuthTokenExecNeverLeaksToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script fixture is POSIX")
	}
	const secret = "gho_TOPSECRET_leakcanary"
	// Print the token to stdout (as gh does), write a benign stderr line, then
	// fail. The token must never appear in the error.
	gh := writeFakeGh(t, "#!/bin/sh\necho "+secret+"\necho 'not logged in' 1>&2\nexit 1\n")
	_, err := ghAuthTokenExec(context.Background(), gh)
	if err == nil {
		t.Fatal("expected an error from a failing gh")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the token: %v", err)
	}
}

// writeFakeGh writes an executable fake gh script and returns its path.
func writeFakeGh(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
