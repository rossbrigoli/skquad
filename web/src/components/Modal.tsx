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
  wide = false,
  wider = false,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  danger?: boolean;
  // S-186: wide dialogs (e.g. the effective-prompt preview) get ~70vw so
  // long prompt text is readable without cramped line lengths.
  wide?: boolean;
  // S-215: the New Squad / New Agent create dialogs get 50% more width
  // (520px → 780px) so the prompt textarea and template controls breathe,
  // while staying responsive on small viewports.
  wider?: boolean;
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
        <div className={`modal-card${danger ? " modal-danger" : ""}${wide ? " modal-wide" : ""}${wider ? " modal-wider" : ""}`}>
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
  testArea,
}: {
  onSubmit: () => void | Promise<void>;
  children: ReactNode;
  submitLabel?: string;
  submitDisabled?: boolean;
  busy?: boolean;
  error?: string;
  onCancel: () => void;
  // S-180 follow-up: optional left-aligned slot in the footer for the
  // pre-save "Test" button + status, so it sits in the same button row
  // as Cancel/Save instead of floating between form fields.
  testArea?: ReactNode;
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
        {testArea ? <div className="modal-test-slot">{testArea}</div> : null}
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
