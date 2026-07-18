import { useCallback, useEffect, useState } from 'react';
import type { ApiClient } from '@/lib/apiClient';
import { ApiError } from '@/lib/apiClient';
import type { AgentInfo, AsrInfo, SecretsPresence, Settings, ToolsInfo } from '@/api/types';
import { ChangePasswordForm } from './settings/ChangePasswordForm';
import { SecretRow } from './settings/SecretRow';
import { AgentSettingsForm } from './settings/AgentSettingsForm';
import { ContributionSettingsForm } from './settings/ContributionSettingsForm';
import { SupervisorSettingsForm } from './settings/SupervisorSettingsForm';

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
  const [tools, setTools] = useState<ToolsInfo | null>(null);
  const [asr, setAsr] = useState<AsrInfo | null>(null);
  const [agentInfo, setAgentInfo] = useState<AgentInfo | null>(null);
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

  // The resolved media-tool paths and ASR capability live on /system (read-only
  // diagnostics), so fetch them once on mount alongside the settings load.
  useEffect(() => {
    let cancelled = false;
    client
      .system()
      .then((info) => {
        if (!cancelled) {
          setTools(info.tools);
          setAsr(info.asr);
          setAgentInfo(info.agent);
        }
      })
      .catch(() => {
        // A tools read failure is non-fatal; the block simply shows nothing.
      });
    return () => {
      cancelled = true;
    };
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
        <SectionTitle>Media tools</SectionTitle>
        <p className="mb-4 max-w-prose text-sm text-dim">
          Resolved at startup (an explicit path, a copy next to the daemon, $PATH, or an
          auto-downloaded build). The audio stages need both; a missing tool fails those books while
          the rest of the daemon keeps working.
        </p>
        <dl className="grid grid-cols-[max-content_1fr] items-center gap-x-6 gap-y-2 text-sm">
          <ToolRow label="ffmpeg" path={tools?.ffmpeg} />
          <ToolRow label="ffprobe" path={tools?.ffprobe} />
          {asr && <AsrRow asr={asr} />}
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
        <AgentSettingsForm client={client} initial={settings.agent} info={agentInfo} />
      </Card>

      <Card>
        <SectionTitle>Contribution</SectionTitle>
        <ContributionSettingsForm client={client} initial={settings.contribution} />
      </Card>

      <Card>
        <SectionTitle>Batch supervisor</SectionTitle>
        <SupervisorSettingsForm client={client} initial={settings.supervisor} />
      </Card>
    </div>
  );
}

// ToolRow shows a resolved tool path, or a muted "Not found" when the daemon
// could not locate it.
function ToolRow({ label, path }: { label: string; path?: string }) {
  return (
    <>
      <dt className="text-dim">{label}</dt>
      <dd className={path ? 'break-all font-mono text-body' : 'text-sm text-dim'}>
        {path ? path : 'Not found'}
      </dd>
    </>
  );
}

// AsrRow shows the resolved speech-recognition backend and, when it will run,
// the detected device; when unavailable it shows the muted reason detail.
function AsrRow({ asr }: { asr: AsrInfo }) {
  return (
    <>
      <dt className="text-dim">ASR</dt>
      {asr.available ? (
        <dd className="break-all font-mono text-body">
          {asr.device ? `${asr.backend} (${asr.device})` : asr.backend}
        </dd>
      ) : (
        <dd className="text-sm text-dim">{asr.detail || 'Not available'}</dd>
      )}
    </>
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
