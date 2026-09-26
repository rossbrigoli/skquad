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
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    // Native <dialog>: showModal() moves focus into the dialog and traps it;
    // Escape fires a cancel event (handled below), backdrop click is caught
    // by the mousedown target check on the dialog element itself.
    ref.current?.showModal();
  }, []);

  return (
    <dialog
      ref={ref}
      className="modal-backdrop"
      aria-label={title}
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
    >
      <div
        role="presentation"
        className="modal-backdrop-inner"
        onMouseDown={(event) => {
          if (event.target === event.currentTarget) onClose();
        }}
      >
        <div className={`modal-card${danger ? " modal-danger" : ""}`}>
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
    </dialog>
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
        Promise.resolve(onSubmit()).catch(() => undefined);
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
