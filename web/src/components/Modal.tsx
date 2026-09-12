"use client";

import { FormEvent, ReactNode, useEffect, useId, useRef, useState } from "react";

// Create forms open in a native <dialog> through showModal(), which provides
// focus trapping, Escape handling, an inert page behind it and top-layer
// rendering without a UI library or z-index juggling.
export function Modal({
  open,
  title,
  onClose,
  onSubmit,
  submitLabel,
  pending = false,
  error = "",
  submitDisabled = false,
  triggerRef,
  children,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  submitLabel: string;
  pending?: boolean;
  error?: string;
  submitDisabled?: boolean;
  triggerRef?: { current: HTMLElement | null };
  children: ReactNode;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);
  const latest = useRef({ open, pending, onClose });
  const titleId = useId();

  // The dialog listeners are attached once, so they read current props here.
  useEffect(() => {
    latest.current = { open, pending, onClose };
  });

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) {
      return;
    }
    // Escape fires "cancel". Route it through onClose so React state stays the
    // source of truth, and refuse to close while a save is in flight.
    const handleCancel = (event: Event) => {
      event.preventDefault();
      if (!latest.current.pending) {
        latest.current.onClose();
      }
    };
    // Browsers can still close a dialog after a prevented cancel. If that
    // happens, tell the parent so it never believes a hidden dialog is open.
    const handleClose = () => {
      if (latest.current.open) {
        latest.current.onClose();
      }
      returnFocusRef.current?.focus();
      returnFocusRef.current = null;
    };
    dialog.addEventListener("cancel", handleCancel);
    dialog.addEventListener("close", handleClose);
    return () => {
      dialog.removeEventListener("cancel", handleCancel);
      dialog.removeEventListener("close", handleClose);
    };
  }, []);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) {
      return;
    }
    if (open && !dialog.open) {
      // Prefer the control that opened the dialog: Safari does not focus a
      // button when it is clicked, so the active element may just be <body>.
      const active = document.activeElement;
      returnFocusRef.current = triggerRef?.current
        ?? (active instanceof HTMLElement && active !== document.body ? active : null);
      dialog.showModal();
      // showModal() focuses the first focusable element, which is the close
      // button; the first editable field is where the user wants to start.
      dialog
        .querySelector<HTMLElement>("input:not([readonly]):not([disabled]), select:not([disabled]), textarea:not([disabled])")
        ?.focus();
    } else if (!open && dialog.open) {
      dialog.close();
    }
  }, [open, triggerRef]);

  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby={titleId}>
      <form className="modal-card" onSubmit={onSubmit}>
        <header className="modal-head">
          <h2 id={titleId}>{title}</h2>
          <button type="button" className="modal-close" aria-label="Close" onClick={onClose} disabled={pending}>
            ×
          </button>
        </header>
        <div className="modal-body">
          {error && (
            <div className="notice error compact" role="alert">
              {error}
            </div>
          )}
          {children}
        </div>
        <footer className="modal-foot">
          <button type="button" className="secondary" onClick={onClose} disabled={pending}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={pending || submitDisabled}>
            {pending ? "Saving…" : submitLabel}
          </button>
        </footer>
      </form>
    </dialog>
  );
}

// Keeps a dialog open until its action succeeds. The action returns an error
// message (shown inside the dialog, input intact) or null to close it.
export function useModalForm(action: () => Promise<string | null>) {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  // State updates land after the event, so two submits in the same tick would
  // both see pending as false; a ref closes that window.
  const inFlight = useRef(false);
  const triggerRef = useRef<HTMLElement | null>(null);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (inFlight.current) {
      return;
    }
    inFlight.current = true;
    setPending(true);
    setError("");
    try {
      const failure = await action();
      if (failure) {
        setError(failure);
      } else {
        setOpen(false);
      }
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Request failed");
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function close() {
    setOpen(false);
    setError("");
  }

  return {
    // Used as the trigger's onClick, so the dialog can hand focus back to that
    // exact control when it closes.
    show: (event?: { currentTarget: EventTarget | null }) => {
      triggerRef.current = event?.currentTarget instanceof HTMLElement ? event.currentTarget : null;
      setError("");
      setOpen(true);
    },
    props: { open, pending, error, onClose: close, onSubmit: submit, triggerRef },
  };
}

export function ListHeader({ title, detail, children }: { title: string; detail?: string; children?: ReactNode }) {
  return (
    <div className="list-head">
      <div>
        <h3 className="panel-title">{title}</h3>
        {detail && <small className="list-detail">{detail}</small>}
      </div>
      {children && <div className="list-actions">{children}</div>}
    </div>
  );
}
