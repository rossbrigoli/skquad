"use client";

import { useEffect, useRef, type FormEvent, type ReactNode } from "react";

// Minimal accessible modal: backdrop click + Escape close, focus moved into the
// dialog on open. Forms inside should use their own submit handling.
export function Modal({
  title,
  onClose,
  children,
  footer,
  danger = false,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  danger?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    ref.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div
        className={`modal-card${danger ? " modal-danger" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        ref={ref}
      >
        <div className="modal-head">
          <h2>{title}</h2>
          <button type="button" className="icon-btn" aria-label="Close" onClick={onClose}>
            ×
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-foot">{footer}</div> : null}
      </div>
    </div>
  );
}

// Submit wrapper that funnels Enter/submit into an async handler with a busy flag.
export function ModalForm({
  onSubmit,
  children,
  submitLabel = "Save",
  submitDisabled = false,
  busy = false,
  error = "",
  onCancel,
}: {
  onSubmit: () => void | Promise<void>;
  children: ReactNode;
  submitLabel?: string;
  submitDisabled?: boolean;
  busy?: boolean;
  error?: string;
  onCancel: () => void;
}) {
  return (
    <form
      className="modal-form"
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        void onSubmit();
      }}
    >
      {children}
      {error ? <div className="notice error">{error}</div> : null}
      <div className="modal-foot">
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={busy || submitDisabled}>
          {busy ? "Working…" : submitLabel}
        </button>
      </div>
    </form>
  );
}
