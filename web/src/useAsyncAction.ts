import { useCallback, useState } from "react";
import { errorText } from "./errors";

export type RunOptions = {
  /** A write (book, move, workshop action): `writing` stays true until it settles. */
  write?: boolean;
};

/**
 * useAsyncAction runs user-started requests and reports their failure as a sentence. Several may
 * overlap (a grid reload while a booking is in flight): `busy` stays true until every one has
 * settled, and `writing` until every write has, so a fast read finishing first never re-enables
 * Confirm or the inputs under a booking still in flight.
 */
export function useAsyncAction() {
  const [pending, setPending] = useState({ all: 0, writes: 0 });
  const [error, setError] = useState("");

  const run = useCallback(async <T,>(fn: () => Promise<T>, opts?: RunOptions): Promise<T | undefined> => {
    const write = opts?.write ? 1 : 0;
    setPending((p) => ({ all: p.all + 1, writes: p.writes + write }));
    setError("");
    try {
      return await fn();
    } catch (err) {
      // Throttling, a busy server, and no network get a plain sentence (errors.ts).
      setError(errorText(err));
      return undefined;
    } finally {
      setPending((p) => ({ all: p.all - 1, writes: p.writes - write }));
    }
  }, []);

  return { busy: pending.all > 0, writing: pending.writes > 0, error, setError, run };
}
