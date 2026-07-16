import { useState, type FormEvent } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';

interface ChangePasswordFormProps {
  client: ApiClient;
}

type Feedback = { kind: 'ok' | 'error'; text: string } | null;

export function ChangePasswordForm({ client }: ChangePasswordFormProps) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<Feedback>(null);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setFeedback(null);
    setBusy(true);
    try {
      await client.changePassword({ current, new: next });
      setFeedback({ kind: 'ok', text: 'Password changed.' });
      setCurrent('');
      setNext('');
    } catch (err) {
      const text = err instanceof ApiError ? err.message : 'Could not change the password.';
      setFeedback({ kind: 'error', text });
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex max-w-sm flex-col gap-3" noValidate>
      <Field
        id="current-password"
        label="Current password"
        value={current}
        autoComplete="current-password"
        onChange={setCurrent}
      />
      <Field
        id="new-password"
        label="New password"
        value={next}
        autoComplete="new-password"
        onChange={setNext}
      />
      {feedback && (
        <p
          role="alert"
          className={'text-sm ' + (feedback.kind === 'ok' ? 'text-success' : 'text-pink-500')}
        >
          {feedback.text}
        </p>
      )}
      <button
        type="submit"
        disabled={busy || current === '' || next === ''}
        className="w-max rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {busy ? 'Saving...' : 'Change password'}
      </button>
    </form>
  );
}

interface FieldProps {
  id: string;
  label: string;
  value: string;
  autoComplete: string;
  onChange: (value: string) => void;
}

function Field({ id, label, value, autoComplete, onChange }: FieldProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-sm font-medium text-hi">
        {label}
      </label>
      <input
        id={id}
        type="password"
        autoComplete={autoComplete}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-md border border-edge bg-raised px-3 py-2 text-body"
      />
    </div>
  );
}
