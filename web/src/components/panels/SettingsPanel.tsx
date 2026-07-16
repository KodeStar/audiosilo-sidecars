import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { SecretsPresence, Settings } from '@/api/types';
import { ChangePasswordForm } from './settings/ChangePasswordForm';
import { SecretRow } from './settings/SecretRow';

interface SettingsPanelProps {
  client: ApiClient;
}

const SECRET_FIELDS: { key: keyof SecretsPresence; label: string }[] = [
  { key: 'anthropic_api_key', label: 'Anthropic API key' },
  { key: 'openai_api_key', label: 'OpenAI API key' },
  { key: 'github_pat', label: 'GitHub PAT' },
];

export function SettingsPanel({ client }: SettingsPanelProps) {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setSettings(await client.getSettings());
      setLoadError(null);
    } catch (err) {
      if (err instanceof ApiError) {
        setLoadError(err.message);
      } else {
        setLoadError('Could not load settings.');
      }
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  if (loadError) {
    return (
      <Card>
        <p role="alert" className="text-sm text-pink-500">
          {loadError}
        </p>
      </Card>
    );
  }

  if (!settings) {
    return (
      <Card>
        <p className="text-sm text-dim">Loading settings...</p>
      </Card>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <SectionTitle>Daemon</SectionTitle>
        <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
          <dt className="text-dim">Listen address</dt>
          <dd className="font-mono text-body">{settings.listen}</dd>
          <dt className="text-dim">CORS origins</dt>
          <dd className="font-mono text-body">
            {settings.cors_origins.length > 0 ? settings.cors_origins.join(', ') : 'none'}
          </dd>
        </dl>
      </Card>

      <Card>
        <SectionTitle>Change password</SectionTitle>
        <ChangePasswordForm client={client} />
      </Card>

      <Card>
        <SectionTitle>Secrets</SectionTitle>
        <p className="mb-4 max-w-prose text-sm text-dim">
          Stored server-side and never shown back. Enter a value to set it, or use Clear to remove
          it.
        </p>
        <div className="flex flex-col divide-y divide-edge">
          {SECRET_FIELDS.map((field) => (
            <SecretRow
              key={field.key}
              client={client}
              field={field.key}
              label={field.label}
              present={settings.secrets[field.key]}
              onChanged={load}
            />
          ))}
        </div>
      </Card>

      <Card>
        <SectionTitle>ASR configuration</SectionTitle>
        <ComingSoon>
          Speech-recognition backend and device selection bind here in a later milestone.
        </ComingSoon>
      </Card>

      <Card>
        <SectionTitle>Agent configuration</SectionTitle>
        <ComingSoon>
          Agent backend and concurrency settings bind here in a later milestone.
        </ComingSoon>
      </Card>
    </div>
  );
}

function Card({ children }: { children: React.ReactNode }) {
  return <div className="rounded-xl border border-edge bg-surface p-6">{children}</div>;
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h2 className="mb-4 text-lg font-medium text-hi">{children}</h2>;
}

function ComingSoon({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-2">
      <span className="w-max rounded-full border border-edge px-2 py-0.5 text-[10px] uppercase tracking-wide text-dim">
        Coming soon
      </span>
      <p className="max-w-prose text-sm text-dim">{children}</p>
    </div>
  );
}
