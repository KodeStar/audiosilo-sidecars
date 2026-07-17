package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// errTimeout marks a CLI run that exceeded its per-invocation Timeout. The child
// process group is killed before it is returned. errors.Is-able.
var errTimeout = errors.New("agent: cli timed out")

// cliSpec fully describes one CLI invocation. Prompt is fed on stdin. Env is the
// COMPLETE child environment (parent env plus any injected keys); it never appears
// in argv. Timeout 0 means no timeout.
type cliSpec struct {
	path    string
	args    []string
	dir     string
	env     []string
	stdin   string
	timeout time.Duration
}

// runCLI starts the process (never via a shell - argv only), feeds stdin, and
// captures stdout and stderr separately. On the parent ctx being cancelled it
// returns ctx.Err(); on the per-invocation timeout expiring it kills the whole
// child process group and returns errTimeout. setProcGroup/killProcGroup are
// platform files so the group kill degrades to a single-process kill on Windows.
func runCLI(ctx context.Context, spec cliSpec) (stdout, stderr string, err error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if spec.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	}

	// exec.Command with a variable (resolved) path + argv-only args; no shell, so
	// there is no injection surface. The child env carries any injected secret; we
	// never log it.
	// exec.Command (not CommandContext) is deliberate: we manage cancellation via the
	// select below so a timeout can kill the whole process GROUP (killProcGroup), which
	// CommandContext's single-process kill would miss. gosec: resolved CLI path +
	// argv-only, no shell.
	cmd := exec.Command(spec.path, spec.args...) //nolint:gosec,noctx // group-kill managed manually; resolved path + argv-only
	cmd.Dir = spec.dir
	if spec.env != nil {
		cmd.Env = spec.env
	} else {
		cmd.Env = os.Environ()
	}
	cmd.Stdin = strings.NewReader(spec.stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	setProcGroup(cmd)

	if serr := cmd.Start(); serr != nil {
		return "", "", fmt.Errorf("start %s: %w", spec.path, serr)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-runCtx.Done():
		killProcGroup(cmd)
		<-done // reap the killed child
		if ctx.Err() != nil {
			// The PARENT context was cancelled (shutdown / stage cancel).
			return outBuf.String(), errBuf.String(), ctx.Err()
		}
		// Only the per-invocation deadline fired.
		return outBuf.String(), errBuf.String(), fmt.Errorf("%w after %s", errTimeout, spec.timeout)
	case werr := <-done:
		return outBuf.String(), errBuf.String(), werr
	}
}

// childEnv returns the parent environment plus KEY=value for each entry in extra.
// The values are the injected API keys; callers must ensure they never reach argv,
// errors, or logs.
func childEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		if v == "" {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

// runVersion runs `<path> <arg>` (typically --version) with a short timeout and
// returns its trimmed combined output, for Detect.
func runVersion(ctx context.Context, path, arg string) (string, error) {
	out, _, err := runCLI(ctx, cliSpec{
		path:    path,
		args:    []string{arg},
		timeout: 15 * time.Second,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
