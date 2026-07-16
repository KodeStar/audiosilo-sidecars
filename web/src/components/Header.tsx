import type { EventStreamState } from '@/lib/useEventStream';
import { Wordmark } from './Wordmark';
import { ConnectionDot } from './ConnectionDot';

interface HeaderProps {
  stream: EventStreamState;
  onSignOut: () => void;
  signingOut: boolean;
}

export function Header({ stream, onSignOut, signingOut }: HeaderProps) {
  return (
    <header className="flex items-center justify-between border-b border-edge bg-surface px-6 py-3">
      <Wordmark />
      <div className="flex items-center gap-5">
        <ConnectionDot status={stream.status} lastHeartbeat={stream.lastHeartbeat} />
        <button
          type="button"
          onClick={onSignOut}
          disabled={signingOut}
          className="rounded-md border border-edge px-3 py-1.5 text-sm text-body transition-colors hover:bg-raised disabled:opacity-50"
        >
          {signingOut ? 'Signing out...' : 'Sign out'}
        </button>
      </div>
    </header>
  );
}
