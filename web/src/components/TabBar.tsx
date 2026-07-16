import type { SystemTab } from '@/api/types';

interface TabBarProps {
  tabs: SystemTab[];
  active: string;
  onSelect: (id: string) => void;
}

export function TabBar({ tabs, active, onSelect }: TabBarProps) {
  return (
    <nav
      className="flex gap-1 border-b border-edge bg-surface px-4"
      role="tablist"
      aria-label="Sections"
    >
      {tabs.map((tab) => {
        const selected = tab.id === active;
        return (
          <button
            key={tab.id}
            type="button"
            role="tab"
            aria-selected={selected}
            onClick={() => onSelect(tab.id)}
            className={
              'flex items-center gap-2 border-b-2 px-4 py-3 text-sm transition-colors ' +
              (selected ? 'border-pink-600 text-hi' : 'border-transparent text-dim hover:text-body')
            }
          >
            {tab.label}
            {tab.status === 'planned' && (
              <span className="rounded-full border border-edge px-2 py-0.5 text-[10px] uppercase tracking-wide text-dim">
                Planned
              </span>
            )}
          </button>
        );
      })}
    </nav>
  );
}
