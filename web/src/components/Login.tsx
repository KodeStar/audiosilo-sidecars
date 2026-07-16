import { useState, type FormEvent } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import { Wordmark } from './Wordmark';

interface LoginProps {
  client: ApiClient;
  onSuccess: (token: string) => void;
}

export function Login({ client, onSuccess }: LoginProps) {
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const { token } = await client.login(password);
      onSuccess(token);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError('Incorrect password.');
      } else if (err instanceof ApiError) {
        setError(err.message);
      } else {
        setError('Could not reach the daemon.');
      }
      setBusy(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-sm rounded-xl border border-edge bg-surface p-8 shadow-lg">
        <div className="mb-6 flex justify-center">
          <Wordmark size="lg" />
        </div>
        <form onSubmit={handleSubmit} className="flex flex-col gap-4" noValidate>
          <div className="flex flex-col gap-2">
            <label htmlFor="password" className="text-sm font-medium text-hi">
              Password
            </label>
            <input
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus
              className="rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
            />
          </div>

          {error && (
            <p role="alert" className="text-sm text-pink-500">
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={busy || password === ''}
            className="rounded-md bg-pink-600 px-4 py-2 font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy ? 'Signing in...' : 'Sign in'}
          </button>

          <p className="text-center text-xs text-dim">
            The one-time password was printed in the daemon&apos;s startup log.
          </p>
        </form>
      </div>
    </main>
  );
}
