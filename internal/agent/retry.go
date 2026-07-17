package agent

import (
	"context"
	"errors"
	"time"
)

// maxOutputRetries is the number of RETRIES (not total attempts) allowed for an
// output that fails the caller's validator: 2 retries = 3 attempts total. Each
// retry appends the validator's error text to the prompt so the agent can self-
// correct.
const maxOutputRetries = 2

// DefaultBackoff is the rate-limit backoff schedule: after a rate-limit failure the
// runner sleeps for the next delay (context-cancellable) and retries WITHOUT
// consuming an output retry. After len(DefaultBackoff) rate-limit rounds the
// RateLimitError is returned so the stage parks.
func DefaultBackoff() []time.Duration {
	return []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
}

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
//   - A validator failure appends the error text to the prompt and retries, up to
//     maxOutputRetries; when exhausted the last Result and the validator error are
//     returned.
//   - A *RateLimitError sleeps for the next backoff delay (context-cancellable) and
//     retries, NOT counting against the output-retry budget; after len(backoff)
//     rate-limit rounds the RateLimitError is returned.
//   - Any other error (including a *NotAvailableError or a timeout) fails immediately.
func RunWithBackoff(ctx context.Context, r Runner, req Request, validate func(Result) error, onUsage func(Usage), backoff []time.Duration) (Result, time.Duration, error) {
	basePrompt := req.Prompt
	prompt := basePrompt
	outputRetries := 0
	rateLimitRounds := 0
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
			return Result{}, slept, err
		}

		if verr := validate(res); verr != nil {
			if outputRetries >= maxOutputRetries {
				return res, slept, verr
			}
			outputRetries++
			prompt = basePrompt + "\n\nYour previous attempt failed validation:\n" + verr.Error() +
				"\nFix the problems and produce the outputs again."
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
