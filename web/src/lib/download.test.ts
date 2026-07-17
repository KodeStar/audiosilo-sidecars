import { describe, it, expect, vi, afterEach } from 'vitest';
import { parseContentDispositionFilename, triggerBlobDownload } from './download';

describe('parseContentDispositionFilename', () => {
  it('extracts a quoted filename', () => {
    expect(
      parseContentDispositionFilename(
        'attachment; filename="the-work-sidecars.zip"',
        'fallback.zip',
      ),
    ).toBe('the-work-sidecars.zip');
  });

  it('extracts an unquoted filename', () => {
    expect(parseContentDispositionFilename('attachment; filename=book.zip', 'fallback.zip')).toBe(
      'book.zip',
    );
  });

  it('prefers and percent-decodes the RFC 5987 extended form', () => {
    expect(
      parseContentDispositionFilename(
        'attachment; filename="fallback.zip"; filename*=UTF-8\'\'the%20work.zip',
        'x.zip',
      ),
    ).toBe('the work.zip');
  });

  it('falls back when the header is null or has no filename', () => {
    expect(parseContentDispositionFilename(null, 'fallback.zip')).toBe('fallback.zip');
    expect(parseContentDispositionFilename('attachment', 'fallback.zip')).toBe('fallback.zip');
  });
});

describe('triggerBlobDownload', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    delete (URL as { createObjectURL?: unknown }).createObjectURL;
    delete (URL as { revokeObjectURL?: unknown }).revokeObjectURL;
  });

  it('defers revokeObjectURL until after the delay so an async download is not aborted', () => {
    vi.useFakeTimers();
    // jsdom does not implement the object-URL API, so assign the mocks directly
    // (spyOn requires the property to already exist); afterEach removes them.
    const createSpy = vi.fn(() => 'blob:mock-url');
    const revokeSpy = vi.fn();
    (URL as { createObjectURL: unknown }).createObjectURL = createSpy;
    (URL as { revokeObjectURL: unknown }).revokeObjectURL = revokeSpy;
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});

    triggerBlobDownload(new Blob(['x']), 'the-work-sidecars.zip');

    // The click fires synchronously, but the revoke must NOT have run yet.
    expect(clickSpy).toHaveBeenCalledTimes(1);
    expect(createSpy).toHaveBeenCalledTimes(1);
    expect(revokeSpy).not.toHaveBeenCalled();

    // The transient anchor is removed synchronously.
    expect(document.querySelector('a[download]')).toBeNull();

    // After the deferred delay elapses, the URL is revoked exactly once.
    vi.advanceTimersByTime(10_000);
    expect(revokeSpy).toHaveBeenCalledTimes(1);
    expect(revokeSpy).toHaveBeenCalledWith('blob:mock-url');
  });
});
