import { describe, it, expect } from 'vitest';
import { PIPELINE_EVENTS } from './useEventStream';

// Drift guard: the daemon publishes these named SSE frames (see internal/scheduler's
// publish sites). A type missing from PIPELINE_EVENTS gets no addEventListener, so the
// browser silently discards those frames. stage.note in particular is the liveness
// signal a long agent stage relies on (it emits no stage.progress), so its absence
// regressed the live log once - keep it registered.
describe('PIPELINE_EVENTS', () => {
  it('registers every published pipeline event type', () => {
    expect(PIPELINE_EVENTS).toEqual([
      'book.state',
      'stage.progress',
      'stage.note',
      'queue.stats',
      'eta.update',
      'contrib.update',
      'supervisor.decision',
    ]);
  });
});
