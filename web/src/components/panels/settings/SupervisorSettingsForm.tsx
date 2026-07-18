import { useState } from 'react';
import type { SupervisorConfig } from '@/api/types';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';

export function SupervisorSettingsForm({
  client,
  initial,
}: {
  client: ApiClient;
  initial: SupervisorConfig;
}) {
  const [form, setForm] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<string | null>(null);

  async function save() {
    setBusy(true);
    setFeedback(null);
    try {
      const updated = await client.updateSettings({ supervisor: form });
      setForm(updated.supervisor);
      setFeedback('Saved. Restart the daemon to apply supervisor changes.');
    } catch (err) {
      setFeedback(err instanceof ApiError ? err.message : 'Could not save supervisor settings.');
    } finally {
      setBusy(false);
    }
  }

  const toggle = (key: keyof SupervisorConfig) =>
    setForm((value) => ({ ...value, [key]: !value[key] }));

  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-prose text-sm text-dim">
        Deterministic checks are cheap and bounded. Automatic recovery, model diagnosis, model
        actions, and backend failover each require an explicit opt-in. The model cannot edit code,
        prompts, facts, documents, budgets, or published outputs.
      </p>
      {(
        [
          ['enabled', 'Enable deterministic monitoring'],
          ['automatic_actions', 'Allow predefined automatic recovery playbooks'],
          ['model_assisted', 'Enable event-driven model diagnosis'],
          ['model_automatic_actions', 'Allow safe model-recommended actions'],
          ['allow_backend_failover', 'Allow the configured fallback backend'],
        ] as [keyof SupervisorConfig, string][]
      ).map(([key, label]) => (
        <label key={key} className="flex w-max max-w-full items-center gap-2 text-sm text-body">
          <input
            type="checkbox"
            checked={form[key]}
            onChange={() => toggle(key)}
            className="h-4 w-4 rounded border-edge bg-raised"
          />
          {label}
        </label>
      ))}
      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          disabled={busy}
          onClick={() => void save()}
          className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white hover:bg-pink-700 disabled:opacity-50"
        >
          Save supervisor settings
        </button>
        {feedback && (
          <p role="status" className="text-xs text-dim">
            {feedback}
          </p>
        )}
      </div>
    </div>
  );
}
