import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { throttleTrailing, useThrottledCallback } from './throttleTrailing';

describe('throttleTrailing', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('fires the leading call immediately', () => {
    const fn = vi.fn();
    const throttled = throttleTrailing(fn, 1000);
    throttled();
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it('coalesces a burst into one trailing call at the window end', () => {
    const fn = vi.fn();
    const throttled = throttleTrailing(fn, 1000);

    throttled(); // leading -> fires now
    expect(fn).toHaveBeenCalledTimes(1);

    // A burst inside the window: no extra immediate fires, just one queued trailing.
    throttled();
    throttled();
    throttled();
    expect(fn).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(2);

    // Nothing more fires without a fresh call.
    vi.advanceTimersByTime(5000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('allows a later call to lead again once the window has elapsed', () => {
    const fn = vi.fn();
    const throttled = throttleTrailing(fn, 1000);

    throttled(); // leading
    vi.advanceTimersByTime(1000);
    throttled(); // window elapsed -> leads again immediately
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it('cancel() prevents a queued trailing call from firing', () => {
    const fn = vi.fn();
    const throttled = throttleTrailing(fn, 1000);

    throttled(); // leading
    throttled(); // queues a trailing
    throttled.cancel();

    vi.advanceTimersByTime(5000);
    expect(fn).toHaveBeenCalledTimes(1); // trailing never fired
  });
});

describe('useThrottledCallback', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('cancels the pending trailing call on unmount', () => {
    const fn = vi.fn();
    const { result, unmount } = renderHook(() => useThrottledCallback(fn, 1000));

    result.current(); // leading
    result.current(); // queues a trailing
    expect(fn).toHaveBeenCalledTimes(1);

    unmount();
    vi.advanceTimersByTime(5000);
    expect(fn).toHaveBeenCalledTimes(1); // no fire after unmount
  });
});
