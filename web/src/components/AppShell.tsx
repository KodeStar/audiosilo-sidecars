import { useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import type { SystemInfo } from '@/api/types';
import { useEventStream } from '@/lib/useEventStream';
import { readTabFromURL, resolveTab, writeTabToURL } from '@/lib/tabNavigation';
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
        const next = resolveTab(info.tabs, readTabFromURL());
        setActive(next);
        writeTabToURL(next, 'replace');
      })
      .catch(() => {
        // A 401 is handled by the client's onAuthError (bounces to login).
        // Any other failure leaves the shell without a tab list.
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  useEffect(() => {
    if (!system) return;
    const handlePopState = () => setActive(resolveTab(system.tabs, readTabFromURL()));
    window.addEventListener('popstate', handlePopState);
    return () => window.removeEventListener('popstate', handlePopState);
  }, [system]);

  function selectTab(id: string) {
    if (id === active) return;
    setActive(id);
    writeTabToURL(id, 'push');
  }

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
      {tabs.length > 0 && <TabBar tabs={tabs} active={active} onSelect={selectTab} />}
      <main className="mx-auto w-full max-w-4xl flex-1 p-6">
        {renderPanel(active, {
          client,
          apiBase,
          token,
          goToRunning: () => selectTab('running'),
        })}
      </main>
      {system && (
        <footer className="border-t border-edge px-6 py-3 text-xs text-dim">
          v{system.version} - {system.listen} - {system.data_dir}
        </footer>
      )}
    </div>
  );
}

interface PanelContext {
  client: ApiClient;
  apiBase: string;
  token: string;
  goToRunning: () => void;
}

function renderPanel(active: string, ctx: PanelContext) {
  switch (active) {
    case 'library':
      return <LibraryPanel client={ctx.client} onProcessed={ctx.goToRunning} />;
    case 'running':
      return <RunningPanel client={ctx.client} apiBase={ctx.apiBase} token={ctx.token} />;
    case 'done':
      return <DonePanel client={ctx.client} apiBase={ctx.apiBase} token={ctx.token} />;
    case 'settings':
      return <SettingsPanel client={ctx.client} />;
    default:
      return <SettingsPanel client={ctx.client} />;
  }
}
