package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// writeSleepScript writes an executable POSIX script that sleeps for the given
// whole-seconds-or-fractional duration then exits 0, and returns its path. It is a
// minimal stand-in for a genuinely-running agent subprocess.
func writeSleepScript(t *testing.T, sleepArg string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("heartbeat test uses a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sleeper")
	script := "#!/bin/sh\nsleep " + sleepArg + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test helper script
		t.Fatal(err)
	}
	return path
}

// TestRunCLIHeartbeatFiresWhileRunningAndStops proves the liveness heartbeat is a REAL
// signal: it fires at least once WHILE the child process runs (interval lowered so a
// ~150ms process spans several ticks) and never fires again once runCLI returns.
func TestRunCLIHeartbeatFiresWhileRunningAndStops(t *testing.T) {
	restore := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = restore })

	var beats atomic.Int64
	var lastElapsed atomic.Int64
	path := writeSleepScript(t, "0.15")

	_, _, err := runCLI(context.Background(), cliSpec{
		path: path,
		heartbeat: func(elapsed time.Duration) {
			beats.Add(1)
			lastElapsed.Store(int64(elapsed))
		},
	})
	if err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	got := beats.Load()
	if got < 1 {
		t.Fatalf("heartbeat fired %d times, want >= 1 while the process ran", got)
	}
	if lastElapsed.Load() <= 0 {
		t.Errorf("heartbeat elapsed = %d, want a positive elapsed", lastElapsed.Load())
	}

	// The ticker must stop the moment runCLI returns: no further beats after the call.
	time.Sleep(80 * time.Millisecond)
	if after := beats.Load(); after != got {
		t.Errorf("heartbeat fired %d more time(s) after runCLI returned; the ticker did not stop", after-got)
	}
}

// TestRunCLIHeartbeatSilentWhenFast confirms the heartbeat does not fire for a process
// that finishes well within one interval (best-effort: the process exits near-instantly
// against a generous 500ms interval).
func TestRunCLIHeartbeatSilentWhenFast(t *testing.T) {
	restore := heartbeatInterval
	heartbeatInterval = 500 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = restore })

	var beats atomic.Int64
	path := writeSleepScript(t, "0") // exits immediately

	if _, _, err := runCLI(context.Background(), cliSpec{
		path:      path,
		heartbeat: func(time.Duration) { beats.Add(1) },
	}); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if got := beats.Load(); got != 0 {
		t.Errorf("heartbeat fired %d time(s) for a sub-interval process, want 0", got)
	}
}

// TestRunCLINilHeartbeatOK confirms a nil heartbeat is a clean no-op (the common path).
func TestRunCLINilHeartbeatOK(t *testing.T) {
	restore := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = restore })

	path := writeSleepScript(t, "0.05")
	if _, _, err := runCLI(context.Background(), cliSpec{path: path}); err != nil {
		t.Fatalf("runCLI with nil heartbeat: %v", err)
	}
}
