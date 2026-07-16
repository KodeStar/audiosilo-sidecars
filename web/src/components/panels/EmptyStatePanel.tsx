import type { ReactNode } from 'react';

interface EmptyStatePanelProps {
  title: string;
  children: ReactNode;
}

// A tidy empty-state card for the not-yet-built sections.
export function EmptyStatePanel({ title, children }: EmptyStatePanelProps) {
  return (
    <div className="rounded-xl border border-edge bg-surface p-8">
      <h2 className="mb-2 text-lg font-medium text-hi">{title}</h2>
      <p className="max-w-prose text-sm leading-relaxed text-dim">{children}</p>
    </div>
  );
}
