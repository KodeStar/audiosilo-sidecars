import type { ReactNode } from 'react';

// Field is the shared form-label wrapper used across the settings cards and the
// core-proposal modal: a stacked label + control column. `required` adds the pink
// asterisk; `className` extends the wrapper (e.g. a grid col-span).
export function Field({
  label,
  htmlFor,
  required,
  className,
  children,
}: {
  label: string;
  htmlFor: string;
  required?: boolean;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={'flex flex-col gap-1.5' + (className ? ` ${className}` : '')}>
      <label htmlFor={htmlFor} className="text-sm font-medium text-hi">
        {label}
        {required && <span className="text-pink-400"> *</span>}
      </label>
      {children}
    </div>
  );
}
