import { useEffect, useMemo } from 'react';

export interface ThrottledFn {
  (): void;
  // Cancels any pending trailing call (e.g. on unmount).
  cancel: () => void;
}

// throttleTrailing wraps fn so the first call in an idle period fires immediately
// (leading edge) and any further calls within the ms window coalesce into a single
// trailing call at the window's end. It is the extracted form of DonePanel's inline
// contrib.update refetch throttle.
export function throttleTrailing(fn: () => void, ms: number): ThrottledFn {
  let last = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const throttled = (() => {
    const since = Date.now() - last;
    if (since >= ms) {
      last = Date.now();
      fn();
      return;
    }
    if (timer) return; // one already queued
    timer = setTimeout(() => {
      timer = null;
      last = Date.now();
      fn();
    }, ms - since);
  }) as ThrottledFn;

  throttled.cancel = () => {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
  };

  return throttled;
}

// useThrottledCallback exposes throttleTrailing as a stable hook: it rebuilds the
// throttler when fn/ms change and cancels the pending trailing call on unmount.
export function useThrottledCallback(fn: () => void, ms: number): () => void {
  const throttled = useMemo(() => throttleTrailing(fn, ms), [fn, ms]);
  useEffect(() => throttled.cancel, [throttled]);
  return throttled;
}
