import { useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { ContributionConfig } from '@/api/types';
import { Field } from '@/components/ui/Field';
import {
  CONTRIBUTION_MODES,
  contributionConfigToForm,
  contributionFormToUpdate,
  validateContributionForm,
  type ContributionFormState,
} from '@/lib/contributionSettings';

interface ContributionSettingsFormProps {
  client: ApiClient;
  // The loaded contribution config seeds the editable form.
  initial: ContributionConfig;
}

type Feedback = { kind: 'ok' | 'error'; text: string } | null;

// ContributionSettingsForm edits how the contributing stage publishes a book's
// sidecars (issue / PR / local), the target repo, auto-purge, and the intake poll
// interval, saving the whole contribution envelope via PUT /settings. Changes are
// restart-to-apply, so a successful save shows that note.
export function ContributionSettingsForm({ client, initial }: ContributionSettingsFormProps) {
  const [form, setForm] = useState<ContributionFormState>(() => contributionConfigToForm(initial));
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<Feedback>(null);

  function set<K extends keyof ContributionFormState>(key: K, value: ContributionFormState[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  async function save() {
    const hint = validateContributionForm(form);
    if (hint) {
      setFeedback({ kind: 'error', text: hint });
      return;
    }
    setBusy(true);
    setFeedback(null);
    try {
      const updated = await client.updateSettings({ contribution: contributionFormToUpdate(form) });
      // Re-seed from the server's echoed view so the baseline matches storage.
      setForm(contributionConfigToForm(updated.contribution));
      setFeedback({ kind: 'ok', text: 'Saved. Restart the daemon to apply contribution changes.' });
    } catch (err) {
      const text = err instanceof ApiError ? err.message : 'Could not save contribution settings.';
      setFeedback({ kind: 'error', text });
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-5">
      <p className="max-w-prose text-sm text-dim">
        How a finished book&apos;s sidecars are contributed to AudioSilo Meta, and how the intake
        poller tracks them. Changes are saved to config but only take effect after a daemon restart.
      </p>

      <div className="flex flex-wrap gap-6">
        <Field label="Mode" htmlFor="contrib-mode">
          <select
            id="contrib-mode"
            value={form.mode}
            onChange={(e) => set('mode', e.target.value)}
            className="rounded-md border border-edge bg-raised px-3 py-2 text-body"
          >
            {CONTRIBUTION_MODES.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
        </Field>

        <Field label="Poll interval (minutes)" htmlFor="contrib-poll">
          <input
            id="contrib-poll"
            type="number"
            min={1}
            value={form.pollMinutes}
            onChange={(e) => set('pollMinutes', e.target.value)}
            className="w-24 rounded-md border border-edge bg-raised px-3 py-2 text-body"
          />
        </Field>
      </div>

      <Field label="Repository (owner/name)" htmlFor="contrib-repo">
        <input
          id="contrib-repo"
          type="text"
          value={form.repo}
          onChange={(e) => set('repo', e.target.value)}
          placeholder="KodeStar/audiosilo-meta"
          className="w-full max-w-md rounded-md border border-edge bg-raised px-3 py-2 text-body placeholder:text-dim"
        />
      </Field>

      <label className="flex w-max items-center gap-2 text-sm text-body">
        <input
          type="checkbox"
          checked={form.autoPurge}
          onChange={(e) => set('autoPurge', e.target.checked)}
          className="h-4 w-4 rounded border-edge bg-raised"
        />
        Auto-purge scratch when a book reaches done
      </label>

      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          disabled={busy}
          onClick={save}
          className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          Save contribution settings
        </button>
        {feedback && (
          <p
            role="status"
            className={'text-xs ' + (feedback.kind === 'ok' ? 'text-success' : 'text-pink-500')}
          >
            {feedback.text}
          </p>
        )}
      </div>
    </div>
  );
}
