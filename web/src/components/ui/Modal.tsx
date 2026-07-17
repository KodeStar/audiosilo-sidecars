import type { ReactNode } from 'react';

interface ModalProps {
  // Accessible label for the dialog; also fired by the overlay click + close button.
  ariaLabel: string;
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  // Panel max-width utility, e.g. 'max-w-lg' or 'max-w-2xl'.
  maxWidthClass: string;
  // Bottom-margin utility for the header row (the two callers differ: 'mb-3'/'mb-4').
  headerClass?: string;
  // When true the title block gets min-w-0 and the subtitle truncates (for a long
  // dynamic subtitle like a book title).
  truncateTitle?: boolean;
  children: ReactNode;
}

// Modal is the shared chrome for the app's dialogs: a click-to-dismiss overlay, a
// centered panel that stops propagation, and a header row (title + optional subtitle
// + close button). The panel width and header margin are knobs so each caller keeps
// its exact visual output.
export function Modal({
  ariaLabel,
  onClose,
  title,
  subtitle,
  maxWidthClass,
  headerClass = 'mb-4',
  truncateTitle = false,
  children,
}: ModalProps) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 sm:p-8"
      role="dialog"
      aria-modal="true"
      aria-label={ariaLabel}
      onClick={onClose}
    >
      <div
        className={`mt-8 w-full ${maxWidthClass} rounded-xl border border-edge bg-surface p-5 shadow-xl`}
        onClick={(e) => e.stopPropagation()}
      >
        <div className={`${headerClass} flex items-start justify-between gap-3`}>
          <div className={truncateTitle ? 'min-w-0' : undefined}>
            <h3 className="text-base font-medium text-hi">{title}</h3>
            {subtitle !== undefined && (
              <p className={`mt-0.5 ${truncateTitle ? 'truncate ' : ''}text-xs text-dim`}>
                {subtitle}
              </p>
            )}
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded p-1 text-dim transition-colors hover:text-hi"
          >
            &#10005;
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}
