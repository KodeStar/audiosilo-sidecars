import { useEffect, useMemo, useRef, useState } from 'react';
import { ApiClient } from '@/lib/apiClient';
import { fetchApiBase } from '@/lib/apiBase';
import { clearToken, getToken, setToken } from '@/lib/token';
import { Login } from '@/components/Login';
import { AppShell } from '@/components/AppShell';
import { scanStore } from '@/lib/scanStore';

export function App() {
  // API base is resolved once at boot (see fetchApiBase). null = still resolving.
  const [apiBase, setApiBase] = useState<string | null>(null);
  const [token, setTokenState] = useState<string | null>(() => getToken());

  // A ref keeps the client's getToken reading the current token without
  // rebuilding the client on every login/logout.
  const tokenRef = useRef(token);
  tokenRef.current = token;

  useEffect(() => {
    let cancelled = false;
    fetchApiBase().then((base) => {
      if (!cancelled) setApiBase(base);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  const client = useMemo(() => {
    if (apiBase === null) return null;
    return new ApiClient({
      baseUrl: apiBase,
      getToken: () => tokenRef.current,
      onAuthError: () => {
        // A 401 on any authed call: drop the dead token and return to login.
        clearToken();
        setTokenState(null);
      },
    });
  }, [apiBase]);

  function handleLogin(newToken: string) {
    setToken(newToken);
    setTokenState(newToken);
  }

  function handleSignOut() {
    // Clear the module-level scan store so the next user starts clean and the
    // once-per-session reattach latch is re-armed.
    scanStore.reset();
    clearToken();
    setTokenState(null);
  }

  if (apiBase === null || client === null) {
    return (
      <main className="flex min-h-screen items-center justify-center">
        <p className="text-sm text-dim">Loading...</p>
      </main>
    );
  }

  if (!token) {
    return <Login client={client} onSuccess={handleLogin} />;
  }

  return <AppShell client={client} apiBase={apiBase} token={token} onSignOut={handleSignOut} />;
}
