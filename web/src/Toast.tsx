import { useCallback, useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";

/** A short success message that fades on its own (errors stay in the ErrorBanner). */
export function useToast(timeoutMs = 4000) {
  const [message, setMessage] = useState("");
  const timer = useRef<number | undefined>(undefined);

  const show = useCallback(
    (text: string) => {
      window.clearTimeout(timer.current);
      setMessage(text);
      timer.current = window.setTimeout(() => setMessage(""), timeoutMs);
    },
    [timeoutMs],
  );
  const dismiss = useCallback(() => {
    window.clearTimeout(timer.current);
    setMessage("");
  }, []);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  return { message, show, dismiss };
}

/**
 * The toast's live region is always mounted (only its content comes and goes), so a screen reader
 * announces each message: a region inserted together with its text is often not read.
 */
export function Toast({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return (
    <div className="live-region" role="status" aria-live="polite" aria-atomic="true">
      {message ? (
        <div className="toast">
          <span>
            <Icon name="checkCircle" />
            {message}
          </span>
          <button type="button" className="banner-close" aria-label="Dismiss" onClick={onDismiss}>
            <Icon name="x" />
          </button>
        </div>
      ) : null}
    </div>
  );
}
