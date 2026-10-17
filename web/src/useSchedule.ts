import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import { errorText } from "./errors";
import type { Schedule } from "./types";

/**
 * The staff day board of one dealership and date. A response is applied only if it is still the
 * latest request, so switching days or dealerships quickly never shows an older board. A failed
 * load is kept as `error` for the board to show (with a retry) instead of loading forever.
 */
export function useSchedule(enabled: boolean, dealershipId: string, date: string) {
  const [schedule, setSchedule] = useState<Schedule | null>(null);
  const [error, setError] = useState("");
  const seq = useRef(0);
  const params = useRef({ enabled, dealershipId, date });
  params.current = { enabled, dealershipId, date };

  /** silent: live events and the refresh after an action never report a failure. */
  async function refresh(opts?: { silent?: boolean }) {
    const p = params.current;
    if (!p.enabled || !p.dealershipId || !p.date) return;
    const mine = ++seq.current;
    try {
      const result = await api.schedule(p.dealershipId, p.date);
      if (mine !== seq.current) return;
      setSchedule(result);
      setError("");
    } catch (err) {
      if (!opts?.silent && mine === seq.current) setError(errorText(err));
    }
  }

  useEffect(() => {
    // A new day or dealership: drop the old board at once rather than showing it under the new date.
    seq.current++;
    setSchedule(null);
    setError("");
    void refresh();
  }, [enabled, dealershipId, date]);

  return { schedule, error, refresh };
}
