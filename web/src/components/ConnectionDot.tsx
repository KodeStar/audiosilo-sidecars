import { useEffect, useState } from 'react';
import type { EventStreamStatus } from '@/lib/useEventStream';

interface ConnectionDotProps {
  status: EventStreamStatus;
  lastHeartbeat: number | null;
}

// A small status dot that is green and flashes each time an SSE heartbeat
// arrives, and gray when the stream is not open.
export function ConnectionDot({ status, lastHeartbeat }: ConnectionDotProps) {
  const [pulse, setPulse] = useState(false);

  useEffect(() => {
    if (lastHeartbeat === null) return;
    setPulse(true);
    const t = setTimeout(() => setPulse(false), 600);
    return () => clearTimeout(t);
  }, [lastHeartbeat]);

  const connected = status === 'open';
  const label = connected ? 'Connected' : 'Reconnecting';

  return (
    <div className="flex items-center gap-2" aria-live="polite">
      <span
        aria-hidden="true"
        className="inline-block h-2.5 w-2.5 rounded-full"
        style={{
          backgroundColor: connected ? 'var(--color-success)' : 'var(--color-dim)',
          animation: pulse ? 'heartbeat-pulse 0.6s ease-out' : 'none',
        }}
      />
      <span className="text-xs text-dim">{label}</span>
    </div>
  );
}
