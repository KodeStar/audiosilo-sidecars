import { useEffect, useRef, useState } from 'react';

export type EventStreamStatus = 'connecting' | 'open' | 'closed';

// The named SSE events the daemon publishes for the pipeline (besides the
// ephemeral heartbeat). See internal/scheduler's publish sites.
export type PipelineEventType = 'book.state' | 'stage.progress' | 'queue.stats';
const PIPELINE_EVENTS: PipelineEventType[] = ['book.state', 'stage.progress', 'queue.stats'];

export interface EventStreamState {
  status: EventStreamStatus;
  // Monotonic timestamp (performance.now / Date.now ms) of the last heartbeat.
  // Bumped on every heartbeat frame so the UI can pulse the connection dot.
  lastHeartbeat: number | null;
}

export interface EventStreamOptions {
  // Invoked for every pipeline event frame with the parsed JSON payload. Held in
  // a ref internally, so passing a fresh closure each render does NOT reconnect.
  onEvent?: (type: PipelineEventType, data: unknown) => void;
}

// Opens an EventSource to `${apiBase}/api/v1/events?token=${token}` and tracks
// the connection status + the last heartbeat. EventSource reconnects natively
// (and resumes with Last-Event-ID); we surface the status and, optionally, fan
// pipeline events out to onEvent.
export function useEventStream(
  apiBase: string,
  token: string | null,
  options: EventStreamOptions = {},
): EventStreamState {
  const [status, setStatus] = useState<EventStreamStatus>('connecting');
  const [lastHeartbeat, setLastHeartbeat] = useState<number | null>(null);
  const sourceRef = useRef<EventSource | null>(null);

  // Keep the latest onEvent without re-opening the stream on every render.
  const onEventRef = useRef(options.onEvent);
  onEventRef.current = options.onEvent;

  useEffect(() => {
    if (!token) {
      setStatus('closed');
      return;
    }

    setStatus('connecting');
    const url = `${apiBase}/api/v1/events?token=${encodeURIComponent(token)}`;
    const source = new EventSource(url);
    sourceRef.current = source;

    const onOpen = () => setStatus('open');
    const onHeartbeat = () => {
      setStatus('open');
      setLastHeartbeat(Date.now());
    };
    const onError = () => {
      // EventSource retries on its own; reflect the interim state.
      setStatus(source.readyState === EventSource.CLOSED ? 'closed' : 'connecting');
    };

    source.addEventListener('open', onOpen);
    source.addEventListener('heartbeat', onHeartbeat);
    source.addEventListener('error', onError);

    const pipelineListeners = PIPELINE_EVENTS.map((type) => {
      const listener = (e: MessageEvent) => {
        let data: unknown;
        try {
          data = JSON.parse(e.data);
        } catch {
          return; // ignore a malformed frame
        }
        onEventRef.current?.(type, data);
      };
      source.addEventListener(type, listener as EventListener);
      return { type, listener };
    });

    return () => {
      source.removeEventListener('open', onOpen);
      source.removeEventListener('heartbeat', onHeartbeat);
      source.removeEventListener('error', onError);
      for (const { type, listener } of pipelineListeners) {
        source.removeEventListener(type, listener as EventListener);
      }
      source.close();
      sourceRef.current = null;
    };
  }, [apiBase, token]);

  return { status, lastHeartbeat };
}
