package agent

import (
	"context"
	"errors"
	"time"
)

// maxOutputRetries is the number of RETRIES (not total attempts) allowed for an
// output that fails the caller's validator: 2 retries = 3 attempts total. Each
// retry re-issues the base prompt plus a delimited retry section carrying the
// validator's error text, and asks the agent to PATCH its prior output rather than
// regenerate it.
const maxOutputRetries = 2

// DefaultBackoff is the rate-limit backoff schedule: after a rate-limit failure the
// runner sleeps for the next delay (context-cancellable) and retries WITHOUT
// consuming an output retry. After len(DefaultBackoff) rate-limit rounds the
// RateLimitError is returned so the stage parks.
func DefaultBackoff() []time.Duration {
	return []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
}

// notAvailableBackoff is the retry schedule for a TRANSIENT *NotAvailableError: a real
// book once parked agent_unavailable because exec.LookPath("claude") failed for a single
// instant while the Claude CLI auto-updated its own symlink - resolve() runs per
// invocation, so a blip at exactly the wrong moment surfaced as "no backend" and parked
// the book (a NotAvailableError bypasses the rate-limit backoff entirely). Retrying it a
// couple of times (each retry re-runs resolve()) rides out that blip; a genuinely-missing
// CLI still fails in ~75s and parks as before. It is a package var so a test can shrink
// it (agent tests run sequentially, so no data race); production keeps 15s then 60s.
var notAvailableBackoff = []time.Duration{15 * time.Second, 60 * time.Second}

// RunWithRetry runs req through r with the default backoff schedule. See
// RunWithBackoff for the policy. The returned duration is the total time spent
// sleeping in rate-limit backoff, so a caller measuring wall-clock cost can subtract
// it and charge only the productive agent time (validation retries included, backoff
// excluded) to its per-stage rate.
func RunWithRetry(ctx context.Context, r Runner, req Request, validate func(Result) error, onUsage func(Usage)) (Result, time.Duration, error) {
	return RunWithBackoff(ctx, r, req, validate, onUsage, DefaultBackoff())
}

// RunWithBackoff runs req through r, retrying on invalid output and backing off on
// rate limits. The backoff schedule is injectable so tests need not sleep. The second
// return value is the total time slept in rate-limit backoff (see RunWithRetry).
//
// Policy:
//   - onUsage is called after EVERY invocation (success or failure) so spend is
//     captured even on a crash between attempts.
//   - A validator failure retries, up to maxOutputRetries; when exhausted the last
//     Result and the validator error are returned. The retry prompt is the full base
//     prompt (a fresh CLI process needs the task context) plus a delimited retry
//     section that carries the validator error verbatim and asks the agent to PATCH
//     its previous output: read the files still present under out/, make the smallest
//     change that fixes the reported problems (fixing OR deleting the offending
//     entries), and leave the corrected outputs there - NOT rebuild them from scratch.
//     Precondition: the caller must keep req.Dir stable (and its out/ writable) across
//     the call. The loop never re-stages, so each fresh CLI process runs in that same
//     cwd and can read the prior attempt's output. Patching instead of regenerating a
//     whole stage output saves large amounts of output tokens.
//   - A *RateLimitError sleeps for the next backoff delay (context-cancellable) and
//     retries, NOT counting against the output-retry budget; after len(backoff)
//     rate-limit rounds the RateLimitError is returned.
//   - A *NotAvailableError retries a small fixed number of times over notAvailableBackoff
//     (each retry re-runs the backend's per-invocation resolve(), so a transient
//     LookPath blip - a CLI auto-updating its symlink - is ridden out); when that budget
//     is spent the NotAvailableError is returned so the stage parks. Its sleep counts
//     into the slept total (rate-sample exclusion), like the rate-limit backoff.
//   - Any other error (a timeout, a render/transport failure) fails immediately.
func RunWithBackoff(ctx context.Context, r Runner, req Request, validate func(Result) error, onUsage func(Usage), backoff []time.Duration) (Result, time.Duration, error) {
	basePrompt := req.Prompt
	prompt := basePrompt
	outputRetries := 0
	rateLimitRounds := 0
	notAvailRetries := 0
	var slept time.Duration

	for {
		attempt := req
		attempt.Prompt = prompt

		res, err := r.Run(ctx, attempt)
		if onUsage != nil {
			onUsage(res.Usage)
		}

		if err != nil {
			var rl *RateLimitError
			if errors.As(err, &rl) {
				if rateLimitRounds >= len(backoff) {
					return Result{}, slept, err
				}
				delay := backoff[rateLimitRounds]
				rateLimitRounds++
				if werr := sleepCtx(ctx, delay); werr != nil {
					return Result{}, slept, werr
				}
				slept += delay
				continue
			}
			var na *NotAvailableError
			if errors.As(err, &na) {
				if notAvailRetries >= len(notAvailableBackoff) {
					return Result{}, slept, err
				}
				delay := notAvailableBackoff[notAvailRetries]
				notAvailRetries++
				if werr := sleepCtx(ctx, delay); werr != nil {
					return Result{}, slept, werr
				}
				slept += delay
				continue
			}
			return Result{}, slept, err
		}

		if verr := validate(res); verr != nil {
			if outputRetries >= maxOutputRetries {
				return res, slept, verr
			}
			outputRetries++
			prompt = basePrompt + "\n\n---\nRETRY: your previous attempt failed validation:\n" +
				verr.Error() +
				"\n\nYour previous attempt's output files are still present under " + OutDirName +
				"/. Read them, then make the SMALLEST change that fixes the problems reported above" +
				" (fixing OR deleting the offending entries is acceptable). Keep everything else" +
				" unchanged and leave the corrected outputs under " + OutDirName +
				"/. Do not rebuild the outputs from scratch."
			continue
		}

		return res, slept, nil
	}
}

// sleepCtx waits for d or until ctx is done, whichever first. It uses a timer (not
// time.Sleep) so a cancelled context returns immediately.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
