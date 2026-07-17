import { describe, it, expect, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import { useLazyDetail } from './useLazyDetail';

describe('useLazyDetail', () => {
  it('is collapsed and idle before any interaction', () => {
    const getDetail = vi.fn().mockResolvedValue({ v: 1 });
    const { result } = renderHook(() => useLazyDetail(getDetail, 7));
    expect(result.current.expanded).toBe(false);
    expect(result.current.detail).toBeNull();
    expect(result.current.detailState).toBe('idle');
    expect(getDetail).not.toHaveBeenCalled();
  });

  it('fetches the detail on open and exposes it', async () => {
    const getDetail = vi.fn().mockResolvedValue({ v: 42 });
    const { result } = renderHook(() => useLazyDetail(getDetail, 7));

    act(() => result.current.toggle());
    expect(result.current.expanded).toBe(true);
    await waitFor(() => expect(result.current.detailState).toBe('idle'));
    expect(result.current.detail).toEqual({ v: 42 });
    expect(getDetail).toHaveBeenCalledWith(7);
  });

  it('re-fetches on each open and does not fetch on collapse', async () => {
    const getDetail = vi.fn().mockResolvedValue({ v: 1 });
    const { result } = renderHook(() => useLazyDetail(getDetail, 3));

    act(() => result.current.toggle()); // open -> fetch
    await waitFor(() => expect(result.current.detailState).toBe('idle'));
    act(() => result.current.toggle()); // close -> no fetch
    expect(result.current.expanded).toBe(false);
    act(() => result.current.toggle()); // open again -> fetch
    await waitFor(() => expect(getDetail).toHaveBeenCalledTimes(2));
  });

  it('surfaces an error state when the fetch rejects', async () => {
    const getDetail = vi.fn().mockRejectedValue(new Error('boom'));
    const { result } = renderHook(() => useLazyDetail(getDetail, 1));

    act(() => result.current.toggle());
    await waitFor(() => expect(result.current.detailState).toBe('error'));
    expect(result.current.detail).toBeNull();
  });

  it('refresh re-fetches while expanded and is a no-op while collapsed', async () => {
    const getDetail = vi.fn().mockResolvedValue({ v: 1 });
    const { result } = renderHook(() => useLazyDetail(getDetail, 5));

    act(() => result.current.refresh()); // collapsed -> no-op
    expect(getDetail).not.toHaveBeenCalled();

    act(() => result.current.toggle()); // open -> fetch (1)
    await waitFor(() => expect(getDetail).toHaveBeenCalledTimes(1));
    act(() => result.current.refresh()); // expanded -> fetch (2)
    await waitFor(() => expect(getDetail).toHaveBeenCalledTimes(2));
  });
});
