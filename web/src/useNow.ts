import { useEffect, useState } from "react";

/** setTimeout's longest delay (about 24.8 days); later boundaries wait for the minute tick. */
const MAX_TIMEOUT_MS = 2_147_483_647;

/** The first boundary strictly after now, if any. */
export function nextBoundary(boundaries: readonly number[], now: number): number | undefined {
  let next: number | undefined;
  for (const b of boundaries) {
    if (Number.isFinite(b) && b > now && (next === undefined || b < next)) next = b;
  }
  return next;
}

/**
 * The current time, re-read once a minute (the board's "now" line) and, when `boundaries` are given
 * (epoch ms at which something time-gated flips, e.g. a booked start or end), right at the next one,
 * so a button never stays enabled (or disabled) for up to a minute past it.
 */
export function useNow(boundaries: readonly number[] = []): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(id);
  }, []);
  const next = nextBoundary(boundaries, now);
  useEffect(() => {
    if (next === undefined) return;
    const delay = next - Date.now();
    if (delay > MAX_TIMEOUT_MS) return;
    // +1 ms: wake strictly after the boundary (a timer may fire a hair early).
    const id = window.setTimeout(() => setNow(Date.now()), Math.max(0, delay) + 1);
    return () => window.clearTimeout(id);
  }, [next, now]);
  return now;
}
