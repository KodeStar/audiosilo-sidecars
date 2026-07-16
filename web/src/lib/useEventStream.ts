import { useEffect, useRef, useState } from 'react';

export type EventStreamStatus = 'connecting' | 'open' | 'closed';

export interface EventStreamState {
  status: EventStreamStatus;
  // Monotonic timestamp (performance.now / Date.now ms) of the last heartbeat.
  // Bumped on every heartbeat frame so the UI can pulse the connection dot.
  lastHeartbeat: number | null;
}

// Opens an EventSource to `${apiBase}/api/v1/events?token=${token}` and tracks
// the connection status + the last heartbeat. EventSource reconnects natively
// (and resumes with Last-Event-ID); we only surface the status.
export function useEventStream(apiBase: string, token: string | null): EventStreamState {
  const [status, setStatus] = useState<EventStreamStatus>('connecting');
  const [lastHeartbeat, setLastHeartbeat] = useState<number | null>(null);
  const sourceRef = useRef<EventSource | null>(null);

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

    return () => {
      source.removeEventListener('open', onOpen);
      source.removeEventListener('heartbeat', onHeartbeat);
      source.removeEventListener('error', onError);
      source.close();
      sourceRef.current = null;
    };
  }, [apiBase, token]);

  return { status, lastHeartbeat };
}
