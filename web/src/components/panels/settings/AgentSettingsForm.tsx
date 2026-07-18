import { useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { AgentConfig, AgentInfo } from '@/api/types';
import {
  AGENT_BACKENDS,
  AGENT_STAGE_KEYS,
  agentConfigToForm,
  agentFormToUpdate,
  validateAgentForm,
  type AgentFormState,
  type AgentStageKey,
} from '@/lib/agentSettings';
import { stateLabel } from '@/lib/pipelineState';
import { Field } from '@/components/ui/Field';

interface AgentSettingsFormProps {
  client: ApiClient;
  // The loaded agent config seeds the editable form.
  initial: AgentConfig;
  // Live runner availability from /system (null when the read failed).
  info: AgentInfo | null;
}

type Feedback = { kind: 'ok' | 'error'; text: string } | null;

// AgentSettingsForm edits the agent backend / concurrency / timeout and the
// per-stage model routing table, saving the whole agent envelope via PUT /settings.
// Agent changes are restart-to-apply, so a successful save shows that note.
export function AgentSettingsForm({ client, initial, info }: AgentSettingsFormProps) {
  const [form, setForm] = useState<AgentFormState>(() => agentConfigToForm(initial));
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<Feedback>(null);

  function setBackend(backend: string) {
    setForm((f) => ({ ...f, backend }));
  }
  function setQueueConcurrency(queueConcurrency: string) {
    setForm((f) => ({ ...f, queueConcurrency }));
  }
  function setMaxAgentsPerBook(maxAgentsPerBook: string) {
    setForm((f) => ({ ...f, maxAgentsPerBook }));
  }
  function setTimeout(timeoutMinutes: string) {
    setForm((f) => ({ ...f, timeoutMinutes }));
  }
  function setBookBudget(bookBudgetUSD: string) {
    setForm((f) => ({ ...f, bookBudgetUSD }));
  }
  function setModel(stage: AgentStageKey, col: 'claude' | 'openai', value: string) {
    setForm((f) => ({
      ...f,
      models: { ...f.models, [stage]: { ...f.models[stage], [col]: value } },
    }));
  }

  async function save() {
    const hint = validateAgentForm(form);
    if (hint) {
      setFeedback({ kind: 'error', text: hint });
      return;
    }
    setBusy(true);
    setFeedback(null);
    try {
      const updated = await client.updateSettings({ agent: agentFormToUpdate(form) });
      // Re-seed from the server's echoed view so the form baseline matches storage.
      setForm(agentConfigToForm(updated.agent));
      setFeedback({
        kind: 'ok',
        text: 'Saved. Restart the daemon to apply agent changes.',
      });
    } catch (err) {
      const text = err instanceof ApiError ? err.message : 'Could not save agent settings.';
      setFeedback({ kind: 'error', text });
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-5">
      <p className="max-w-prose text-sm text-dim">
        Concurrent books controls breadth across the queue. Max agents per book controls fan-out
        within supported stages. ASR remains serial and series ordering still applies. Changes only
        take effect after a daemon restart.
      </p>

      <AvailabilityLine info={info} />

      <div className="flex flex-wrap gap-6">
        <Field label="Backend" htmlFor="agent-backend">
          <select
            id="agent-backend"
            value={form.backend}
            onChange={(e) => setBackend(e.target.value)}
            className="rounded-md border border-edge bg-raised px-3 py-2 text-body"
          >
            {AGENT_BACKENDS.map((b) => (
              <option key={b.value} value={b.value}>
                {b.label}
              </option>
            ))}
          </select>
        </Field>

        <Field label="Concurrent books" htmlFor="agent-queue-concurrency">
          <input
            id="agent-queue-concurrency"
            type="number"
            min={1}
            value={form.queueConcurrency}
            onChange={(e) => setQueueConcurrency(e.target.value)}
            className="w-24 rounded-md border border-edge bg-raised px-3 py-2 text-body"
          />
        </Field>

        <Field label="Max agents per book" htmlFor="agent-max-per-book">
          <input
            id="agent-max-per-book"
            type="number"
            min={1}
            value={form.maxAgentsPerBook}
            onChange={(e) => setMaxAgentsPerBook(e.target.value)}
            className="w-24 rounded-md border border-edge bg-raised px-3 py-2 text-body"
          />
        </Field>

        <Field label="Timeout (minutes)" htmlFor="agent-timeout">
          <input
            id="agent-timeout"
            type="number"
            min={1}
            value={form.timeoutMinutes}
            onChange={(e) => setTimeout(e.target.value)}
            className="w-24 rounded-md border border-edge bg-raised px-3 py-2 text-body"
          />
        </Field>

        <Field label="Book budget (USD)" htmlFor="agent-budget">
          <input
            id="agent-budget"
            type="number"
            min={0}
            step="0.01"
            value={form.bookBudgetUSD}
            onChange={(e) => setBookBudget(e.target.value)}
            className="w-28 rounded-md border border-edge bg-raised px-3 py-2 text-body"
          />
        </Field>
      </div>

      <p className="text-xs text-dim">
        Maximum possible simultaneous invocations:{' '}
        {Number(form.queueConcurrency) * Number(form.maxAgentsPerBook) || 0}. Fan-out is currently
        supported for fact extraction and chapter-partitioned QA adjudication.
      </p>

      <div className="flex flex-col gap-2">
        <span className="text-sm font-medium text-hi">Per-stage models</span>
        <p className="max-w-prose text-xs text-dim">
          Leave a cell empty to use the backend's default model for that stage. The Claude column
          takes CLI model aliases (sonnet, opus); the OpenAI column takes codex model names.
        </p>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[28rem] border-collapse text-sm">
            <thead>
              <tr className="border-b border-edge text-left text-xs uppercase tracking-wide text-dim">
                <th className="py-2 pr-4 font-medium">Stage</th>
                <th className="py-2 pr-4 font-medium">Claude model</th>
                <th className="py-2 font-medium">OpenAI model</th>
              </tr>
            </thead>
            <tbody>
              {AGENT_STAGE_KEYS.map((stage) => (
                <tr key={stage} className="border-b border-edge/50 last:border-0">
                  <td className="py-2 pr-4 text-body">{stateLabel(stage)}</td>
                  <td className="py-2 pr-4">
                    <ModelCell
                      stage={stage}
                      col="claude"
                      value={form.models[stage].claude}
                      onChange={setModel}
                    />
                  </td>
                  <td className="py-2">
                    <ModelCell
                      stage={stage}
                      col="openai"
                      value={form.models[stage].openai}
                      onChange={setModel}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          disabled={busy}
          onClick={save}
          className="rounded-md bg-pink-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-pink-700 disabled:cursor-not-allowed disabled:opacity-50"
        >
          Save agent settings
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

// AvailabilityLine shows the resolved runner backend from /system: the backend
// name plus available/version, or the muted unavailable detail.
function AvailabilityLine({ info }: { info: AgentInfo | null }) {
  if (!info) return null;
  const backendName = info.backend || 'none detected';
  return (
    <dl className="grid grid-cols-[max-content_1fr] items-center gap-x-6 gap-y-1 text-sm">
      <dt className="text-dim">Detected runner</dt>
      {info.available ? (
        <dd className="break-all font-mono text-body">
          {info.version ? `${backendName} (${info.version})` : backendName}
        </dd>
      ) : (
        <dd className="text-sm text-dim">{info.detail || `${backendName} - not available`}</dd>
      )}
    </dl>
  );
}

function ModelCell({
  stage,
  col,
  value,
  onChange,
}: {
  stage: AgentStageKey;
  col: 'claude' | 'openai';
  value: string;
  onChange: (stage: AgentStageKey, col: 'claude' | 'openai', value: string) => void;
}) {
  return (
    <input
      type="text"
      aria-label={`${stateLabel(stage)} ${col} model`}
      value={value}
      placeholder="default"
      onChange={(e) => onChange(stage, col, e.target.value)}
      className="w-full min-w-[8rem] rounded-md border border-edge bg-raised px-2 py-1.5 text-body placeholder:text-dim"
    />
  );
}
