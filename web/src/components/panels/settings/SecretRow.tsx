import { useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { SecretsPresence } from '@/api/types';

interface SecretRowProps {
  client: ApiClient;
  field: keyof SecretsPresence;
  label: string;
  present: boolean;
  onChanged: () => Promise<void> | void;
}

type Feedback = { kind: 'ok' | 'error'; text: string } | null;

export function SecretRow({ client, field, label, present, onChanged }: SecretRowProps) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<Feedback>(null);

  async function put(secretValue: string, okText: string) {
    setBusy(true);
    setFeedback(null);
    try {
      await client.updateSettings({ secrets: { [field]: secretValue } });
      setValue('');
      setFeedback({ kind: 'ok', text: okText });
      await onChanged();
    } catch (err) {
      const text = err instanceof ApiError ? err.message : 'Could not update the secret.';
      setFeedback({ kind: 'error', text });
    } finally {
      setBusy(false);
    }
  }

  const inputId = `secret-${field}`;

  return (
    <div className="flex flex-col gap-2 py-4 first:pt-0 last:pb-0">
      <div className="flex items-center justify-between gap-3">
        <label htmlFor={inputId} className="text-sm font-medium text-hi">
          {label}
        </label>
        {present ? (
          <span className="rounded-full bg-success/15 px-2 py-0.5 text-xs font-medium text-success">
            Set
          </span>
        ) : (
          <span className="rounded-full border border-edge px-2 py-0.5 text-xs text-dim">
            Not set
          </span>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <input
          id={inputId}
          type="password"
          autoComplete="off"
          value={value}
          placeholder="Enter a new value"
          onChange={(e) => setValue(e.target.value)}
          className="min-w-0 flex-1 rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
        />
        <button
          type="button"
          disabled={busy || value === ''}
          onClick={() => put(value, 'Saved.')}
          className="rounded-md bg-pink-600 px-3 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          Save
        </button>
        <button
          type="button"
          disabled={busy || !present}
          onClick={() => put('', 'Cleared.')}
          className="rounded-md border border-edge px-3 py-2 text-sm text-body transition-colors hover:bg-raised disabled:cursor-not-allowed disabled:opacity-50"
        >
          Clear
        </button>
      </div>
      {feedback && (
        <p
          role="status"
          className={'text-xs ' + (feedback.kind === 'ok' ? 'text-success' : 'text-pink-500')}
        >
          {feedback.text}
        </p>
      )}
    </div>
  );
}
