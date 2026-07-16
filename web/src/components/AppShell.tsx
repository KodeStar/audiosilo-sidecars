import { useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import type { SystemInfo } from '@/api/types';
import { useEventStream } from '@/lib/useEventStream';
import { Header } from './Header';
import { TabBar } from './TabBar';
import { LibraryPanel } from './panels/LibraryPanel';
import { RunningPanel } from './panels/RunningPanel';
import { DonePanel } from './panels/DonePanel';
import { SettingsPanel } from './panels/SettingsPanel';

interface AppShellProps {
  client: ApiClient;
  apiBase: string;
  token: string;
  onSignOut: () => void;
}

export function AppShell({ client, apiBase, token, onSignOut }: AppShellProps) {
  const [system, setSystem] = useState<SystemInfo | null>(null);
  const [active, setActive] = useState<string>('settings');
  const [signingOut, setSigningOut] = useState(false);

  const stream = useEventStream(apiBase, token);

  useEffect(() => {
    let cancelled = false;
    client
      .system()
      .then((info) => {
        if (cancelled) return;
        setSystem(info);
        // Default to the first "ready" tab, else the first tab.
        const ready = info.tabs.find((t) => t.status === 'ready');
        setActive((ready ?? info.tabs[0])?.id ?? 'settings');
      })
      .catch(() => {
        // A 401 is handled by the client's onAuthError (bounces to login).
        // Any other failure leaves the shell without a tab list.
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  async function handleSignOut() {
    setSigningOut(true);
    try {
      await client.logout();
    } catch {
      // Even if the call fails, we clear locally below.
    }
    onSignOut();
  }

  const tabs = system?.tabs ?? [];

  return (
    <div className="flex min-h-screen flex-col">
      <Header stream={stream} onSignOut={handleSignOut} signingOut={signingOut} />
      {tabs.length > 0 && <TabBar tabs={tabs} active={active} onSelect={setActive} />}
      <main className="mx-auto w-full max-w-4xl flex-1 p-6">{renderPanel(active, client)}</main>
      {system && (
        <footer className="border-t border-edge px-6 py-3 text-xs text-dim">
          v{system.version} - {system.listen} - {system.data_dir}
        </footer>
      )}
    </div>
  );
}

function renderPanel(active: string, client: ApiClient) {
  switch (active) {
    case 'library':
      return <LibraryPanel />;
    case 'running':
      return <RunningPanel />;
    case 'done':
      return <DonePanel />;
    case 'settings':
      return <SettingsPanel client={client} />;
    default:
      return <SettingsPanel client={client} />;
  }
}
