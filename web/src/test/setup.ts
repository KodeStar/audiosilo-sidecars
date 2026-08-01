import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// jsdom has no EventSource, so any component using useEventStream depends on a test
// installing one. Provide an inert baseline here rather than leaving the global
// undefined.
//
// Without it the suite is flaky (roughly one run in six). A test that stubs its own
// EventSource restores the PREVIOUS value on teardown, and vitest runs afterEach
// hooks in reverse registration order - so the stub is removed before this file's
// cleanup() unmounts anything. React 19 flushes passive mount effects
// asynchronously, so an effect scheduled by a render can open its EventSource after
// the stub is gone; with the global undefined that throws and fails whichever test
// happens to be running, not the one that rendered. Restoring to an inert class
// makes the late effect a no-op instead.
class InertEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readonly readyState = InertEventSource.CLOSED;
  addEventListener() {}
  removeEventListener() {}
  dispatchEvent() {
    return false;
  }
  close() {}
}
globalThis.EventSource ??= InertEventSource as unknown as typeof EventSource;

afterEach(() => {
  cleanup();
});
