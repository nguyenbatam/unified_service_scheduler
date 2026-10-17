import { useCallback, useState } from "react";
import { adminErrorText } from "./admin";
import { isApiError } from "./api";
import { useAsyncAction } from "./useAsyncAction";

/**
 * useAdminAction runs admin saves: one at a time (busy), failures as readable sentences
 * (adminErrorText), a success notice, and on 412 a reload of what was stale (onStale) so the form
 * shows the latest values instead of overwriting them.
 */
export function useAdminAction(onStale?: () => Promise<unknown> | void) {
  const { busy, error, setError, run } = useAsyncAction();
  const [notice, setNotice] = useState("");

  /** Resolves true when fn succeeded. */
  const save = useCallback(
    async (fn: () => Promise<unknown>, done?: string): Promise<boolean> => {
      setNotice("");
      const ok = await run(async () => {
        try {
          await fn();
          return true;
        } catch (err) {
          if (isApiError(err) && err.status === 412 && onStale) {
            try {
              await onStale();
            } catch {
              // the reload failing too is reported by the 412 message below
            }
          }
          throw new Error(adminErrorText(err));
        }
      });
      if (ok && done) setNotice(done);
      return ok === true;
    },
    [run, onStale],
  );

  return { busy, error, setError, notice, setNotice, save };
}
