import { type KeyboardEvent, useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";

// confirmAction asks the user to confirm a destructive action and resolves with their answer.
// It renders through the single <ConfirmHost /> mounted in App; without a host (unit tests of a
// lone component, or before App mounts) it falls back to window.confirm.
export function confirmAction(message: string): Promise<boolean> {
  if (!show) {
    return Promise.resolve(typeof window !== "undefined" && typeof window.confirm === "function" ? window.confirm(message) : true);
  }
  return show(message);
}

let show: ((message: string) => Promise<boolean>) | null = null;

interface Pending {
  message: string;
  resolve: (ok: boolean) => void;
}

// ConfirmHost renders the dialog for confirmAction; mount it once near the root. Focus starts on
// the safe answer (Enter never confirms by accident), Tab stays inside the dialog, Escape or a
// click outside backs out, and focus returns to whatever opened the dialog.
export function ConfirmHost() {
  const [pending, setPending] = useState<Pending | null>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const opener = useRef<HTMLElement | null>(null);

  useEffect(() => {
    show = (message) =>
      new Promise<boolean>((resolve) => {
        setPending((prev) => {
          prev?.resolve(false);
          if (!prev) opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
          return { message, resolve };
        });
      });
    return () => {
      show = null;
    };
  }, []);

  const open = pending !== null;
  useEffect(() => {
    if (!open) return;
    cancelRef.current?.focus();
    return () => {
      // Back to the button that asked, if it is still on the page.
      const el = opener.current;
      opener.current = null;
      if (el && el.isConnected) el.focus();
    };
  }, [open]);

  function answer(ok: boolean) {
    pending?.resolve(ok);
    setPending(null);
  }

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (e.key === "Escape") {
      e.preventDefault();
      answer(false);
      return;
    }
    if (e.key !== "Tab") return;
    // Two buttons: Tab and Shift+Tab move between them and never leave the dialog.
    e.preventDefault();
    const next = document.activeElement === cancelRef.current ? confirmRef.current : cancelRef.current;
    next?.focus();
  }

  if (!pending) return null;
  return (
    <div className="dialog-backdrop" onClick={() => answer(false)}>
      <div
        className="dialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="confirm-dialog-message"
        onClick={(e) => e.stopPropagation()}
        onKeyDown={onKeyDown}
      >
        <p id="confirm-dialog-message">
          <Icon name="alert" className="icon-warn" />
          {pending.message}
        </p>
        <div className="dialog-actions">
          <button type="button" className="secondary" ref={cancelRef} onClick={() => answer(false)}>
            <Icon name="undo" />
            Go back
          </button>
          <button type="button" className="danger" ref={confirmRef} onClick={() => answer(true)}>
            <Icon name="check" />
            Yes, continue
          </button>
        </div>
      </div>
    </div>
  );
}
