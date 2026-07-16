interface WordmarkProps {
  size?: 'sm' | 'lg';
}

// Inline logo (a small pink waveform glyph) + the product wordmark. No external
// asset is needed.
export function Wordmark({ size = 'sm' }: WordmarkProps) {
  const glyph = size === 'lg' ? 40 : 28;
  return (
    <div className="flex items-center gap-3">
      <Logo size={glyph} />
      <span
        className={size === 'lg' ? 'text-xl font-medium text-hi' : 'text-base font-medium text-hi'}
      >
        AudioSilo <span className="text-pink-600">Sidecars</span>
      </span>
    </div>
  );
}

function Logo({ size }: { size: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 32 32"
      role="img"
      aria-label="AudioSilo Sidecars logo"
      fill="none"
    >
      <rect width="32" height="32" rx="8" fill="var(--color-pink-600)" />
      <g stroke="#ffffff" strokeWidth="2.4" strokeLinecap="round" opacity="0.95">
        <line x1="9" y1="13" x2="9" y2="19" />
        <line x1="14" y1="9" x2="14" y2="23" />
        <line x1="19" y1="11" x2="19" y2="21" />
        <line x1="24" y1="14" x2="24" y2="18" />
      </g>
    </svg>
  );
}
