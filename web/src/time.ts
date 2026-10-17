export function formatClock(iso: string, timeZone: string): string {
  return new Intl.DateTimeFormat("en-GB", {
    timeZone,
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  }).format(new Date(iso));
}

/** YYYY-MM-DD in the given zone (en-CA formats ISO-style). */
export function formatDate(iso: string, timeZone: string): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone, year: "numeric", month: "2-digit", day: "2-digit" }).format(
    new Date(iso),
  );
}

export function formatClockRange(startIso: string, endIso: string, timeZone: string): string {
  return `${formatClock(startIso, timeZone)}–${formatClock(endIso, timeZone)}`;
}

export function parseClockMinutes(hhmm: string): number {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + m;
}

/**
 * Minutes from local midnight of `day` (YYYY-MM-DD in `timeZone`) to the instant `iso`: negative
 * for an earlier day, 1440 or more for a later one (an end at the next midnight is 1440, not 0).
 */
export function minutesOnDay(iso: string, day: string, timeZone: string): number {
  const date = formatDate(iso, timeZone);
  const clock = parseClockMinutes(formatClock(iso, timeZone));
  if (date === day) return clock;
  const dayMs = Date.UTC(...ymd(day));
  const otherMs = Date.UTC(...ymd(date));
  return clock + Math.round((otherMs - dayMs) / 86_400_000) * 1440;
}

function ymd(day: string): [number, number, number] {
  const [y, m, d] = day.split("-").map(Number);
  return [y, m - 1, d];
}

export type BoardLayout = {
  topPct: number;
  heightPct: number;
  /** The block began before the board's first minute (an earlier day, or before opening). */
  clippedStart: boolean;
  /** The block runs past the board's last minute (after closing, or into the next day). */
  clippedEnd: boolean;
  /** Minutes of the block visible on the board (decides how much text fits in it). */
  minutes: number;
};

/**
 * Vertical position of an appointment block on the day board of `day`, which spans open–close.
 * Blocks are clipped to the board: one that started the previous day starts at the top, one held
 * past closing (a flagged overrun, held until completed) reaches the bottom.
 */
export function boardLayout(
  startIso: string,
  endIso: string,
  day: string,
  openTime: string,
  closeTime: string,
  timeZone: string,
): BoardLayout {
  const open = parseClockMinutes(openTime);
  const close = parseClockMinutes(closeTime);
  const span = Math.max(close - open, 1);
  const start = minutesOnDay(startIso, day, timeZone);
  const end = minutesOnDay(endIso, day, timeZone);
  const top = Math.min(Math.max(start, open), close);
  const bottom = Math.min(Math.max(end, open), close);
  return {
    topPct: ((top - open) / span) * 100,
    heightPct: Math.max(2, ((bottom - top) / span) * 100),
    clippedStart: start < open,
    clippedEnd: end > close,
    minutes: bottom - top,
  };
}

/** Position (percent from the top) of a clock time "HH:MM" on a board spanning open–close; null outside. */
export function boardOffset(clock: string | number, openTime: string, closeTime: string): number | null {
  const open = parseClockMinutes(openTime);
  const close = parseClockMinutes(closeTime);
  const at = typeof clock === "number" ? clock : parseClockMinutes(clock);
  if (at < open || at > close || close <= open) return null;
  return ((at - open) / (close - open)) * 100;
}

/** Whole hours shown on a board's time axis (open and close excluded unless on the hour). */
export function boardHours(openTime: string, closeTime: string): number[] {
  const open = parseClockMinutes(openTime);
  const close = parseClockMinutes(closeTime);
  const hours: number[] = [];
  for (let m = Math.ceil(open / 60) * 60; m <= close; m += 60) hours.push(m);
  return hours;
}

/** "HH:MM" for minutes from midnight. */
export function clockOf(minutes: number): string {
  const h = Math.floor(minutes / 60) % 24;
  const m = minutes % 60;
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`;
}

/** Long, readable date ("Mon, 19 Oct 2026") of a YYYY-MM-DD civil date. */
export function formatDayLabel(day: string): string {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return day;
  return new Intl.DateTimeFormat("en-GB", {
    timeZone: "UTC",
    weekday: "short",
    day: "numeric",
    month: "short",
    year: "numeric",
  }).format(new Date(Date.UTC(...ymd(day))));
}

/** The civil date `delta` days from a YYYY-MM-DD date (calendar arithmetic, no time zone). */
export function shiftDay(day: string, delta: number): string {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) return day;
  const [y, m, d] = ymd(day);
  return new Date(Date.UTC(y, m, d + delta)).toISOString().slice(0, 10);
}

export function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && crypto.randomUUID) {
    return crypto.randomUUID();
  }
  return `idem-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

/** Offset (ms) of `timeZone` from UTC at the instant `utcMs`. */
function zoneOffsetMs(utcMs: number, timeZone: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone,
    hourCycle: "h23",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).formatToParts(new Date(utcMs));
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  const asUtc = Date.UTC(get("year"), get("month") - 1, get("day"), get("hour"), get("minute"), get("second"));
  return asUtc - Math.floor(utcMs / 1000) * 1000;
}

/** Wall time "YYYY-MM-DDTHH:mm" (a datetime-local value) in `timeZone` → RFC 3339 UTC. */
export function zonedToIso(local: string, timeZone: string): string {
  const [day, clock = "00:00"] = local.split("T");
  const [y, mo, d] = day.split("-").map(Number);
  const [h, mi] = clock.split(":").map(Number);
  const wall = Date.UTC(y, mo - 1, d, h, mi);
  const guess = wall - zoneOffsetMs(wall, timeZone);
  // Re-read the offset at the guessed instant: it can differ from the wall-clock one near DST.
  return new Date(wall - zoneOffsetMs(guess, timeZone)).toISOString();
}

/** "45m", "2h", "1h30m", "3d" — compact policy durations. */
export function formatMinutes(minutes: number): string {
  if (minutes >= 1440 && minutes % 1440 === 0) return `${minutes / 1440}d`;
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  if (h === 0) return `${m}m`;
  return m === 0 ? `${h}h` : `${h}h${m}m`;
}

/** Short name of `timeZone` at the instant `iso` ("BST", "GMT", "GMT+2"), as Intl writes it. */
export function zoneAbbr(timeZone: string, iso: string | number = Date.now()): string {
  try {
    const parts = new Intl.DateTimeFormat("en-GB", { timeZone, timeZoneName: "short" }).formatToParts(new Date(iso));
    return parts.find((p) => p.type === "timeZoneName")?.value ?? timeZone;
  } catch {
    return timeZone;
  }
}

/**
 * "London time (BST)": the dealership's zone next to the times shown, so a customer elsewhere
 * (or a group admin) knows whose clock they read. "UTC" stays "UTC".
 */
export function zoneLabel(timeZone: string, iso: string | number = Date.now()): string {
  const abbr = zoneAbbr(timeZone, iso);
  const city = timeZone.includes("/") ? timeZone.slice(timeZone.lastIndexOf("/") + 1).replace(/_/g, " ") : timeZone;
  return city === abbr ? abbr : `${city} time (${abbr})`;
}
