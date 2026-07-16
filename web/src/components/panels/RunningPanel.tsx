import { EmptyStatePanel } from './EmptyStatePanel';

export function RunningPanel() {
  return (
    <EmptyStatePanel title="Running">
      Live pipeline progress (stage timeline, ETA, cost) lands in M6.
    </EmptyStatePanel>
  );
}
