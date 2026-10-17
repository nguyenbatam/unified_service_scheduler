import { NetworkError, isApiError } from "./api";

// Readable sentences for failed requests. The server's 4xx messages are written for people, but
// the transport failures (throttling, a busy database, no network) and the booking conflicts need
// a plain explanation and a next step.

const OFFLINE = "Can't reach the server — check your connection and try again.";

/** "in 5 s" from a Retry-After, or "in a moment" without one. */
function retryIn(seconds: number | undefined): string {
  return seconds && seconds > 0 ? `in ${Math.ceil(seconds)} s` : "in a moment";
}

/** Upper-cases the first letter of a server message ("slot overlaps…" → "Slot overlaps…"). */
function sentence(message: string): string {
  const text = message.trim().replace(/\.$/, "");
  return text ? text[0].toUpperCase() + text.slice(1) : text;
}

/**
 * errorText is the banner text of any failure: throttling (429, with the server's Retry-After),
 * a busy scheduler (503), and a request that never reached the server (api.ts's NetworkError) get a
 * plain sentence; everything else keeps the server's message.
 */
export function errorText(err: unknown): string {
  if (err instanceof NetworkError) return OFFLINE;
  if (!isApiError(err)) return err instanceof Error ? err.message : String(err);
  switch (err.status) {
    case 429:
      return `Too many requests — try again ${retryIn(err.retryAfter)}.`;
    case 502:
    case 504:
      return OFFLINE;
    case 503:
      // api.ts already words the "busy" code (a write may have been saved); a gateway 503 is not.
      return err.code === "busy" ? err.message : `The scheduler is busy right now — try again ${retryIn(err.retryAfter)}.`;
    default:
      return err.message;
  }
}

/** A refused booking or move whose slot is stale: the grid is re-read and the user picks again. */
export function isSlotConflict(err: unknown): boolean {
  return isApiError(err) && (err.status === 409 || err.status === 422);
}

const REFRESHED = "slots refreshed, please pick another time.";

/** The API's error codes for refused bookings (domain.Code*): 409 and 422 respectively. */
export const CODE_CONFLICT = "conflict";
export const CODE_UNAVAILABLE = "unavailable";

/**
 * bookingErrorText explains a refused booking or reschedule (409 conflict, 422 rule) once the grid
 * has been re-read: what went wrong, in words, then that the slots were refreshed. The API's `code`
 * decides the kind (`conflict`: taken or clashing, `unavailable`: against the day's rules); the codes
 * are generic, so the message only refines which rule it was, and an unknown message keeps the
 * code's generic sentence.
 */
export function bookingErrorText(err: unknown): string {
  if (!isSlotConflict(err) || !isApiError(err)) return errorText(err);
  const msg = err.message.toLowerCase();
  const kind = err.code === CODE_CONFLICT || err.code === CODE_UNAVAILABLE ? err.code : err.status === 409 ? CODE_CONFLICT : CODE_UNAVAILABLE;
  if (msg.includes("only confirmed appointments")) {
    return "This appointment can no longer be moved (it was checked in, cancelled or marked a no-show).";
  }
  if (kind === CODE_CONFLICT) {
    if (msg.includes("vehicle already has an appointment")) {
      return `This car already has an appointment overlapping that time — ${REFRESHED}`;
    }
    if (msg.includes("idempotency key")) return "That booking request was already used — please pick the time again.";
    return `That time was just taken — ${REFRESHED}`;
  }
  if (msg.includes("staging")) return `The check-in area is full for that arrival time — ${REFRESHED}`;
  if (msg.includes("lunch")) return `That time overlaps the lunch break — ${REFRESHED}`;
  if (msg.includes("opening hours")) return `That time is outside the opening hours — ${REFRESHED}`;
  // Policy refusals ("online bookings need at least 60 minutes notice…") are already worded.
  return msg ? `${sentence(err.message)} — ${REFRESHED}` : `That time can no longer be booked — ${REFRESHED}`;
}

/** actionErrorText explains a refused workshop action (check in, no-show, reassign…). */
export function actionErrorText(err: unknown): string {
  if (!isSlotConflict(err) || !isApiError(err)) return errorText(err);
  return `Not done: ${sentence(err.message)}.`;
}
