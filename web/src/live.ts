import { formatDate } from "./time";
import { AggregateType, EventType } from "./types";
import type { Appointment, AppointmentNotice, AvailabilityNotice, ConfigNotice, EventTypeCode } from "./types";

/** Appointment events that may change what the board or the current appointment shows. */
export const LIVE_APPT_EVENTS: ReadonlySet<number> = new Set<EventTypeCode>([
  EventType.AppointmentCreated,
  EventType.AppointmentCancelled,
  EventType.AppointmentRescheduled,
  EventType.AppointmentCheckedIn,
  EventType.AppointmentStarted,
  EventType.AppointmentCompleted,
  EventType.AppointmentNoShow,
  EventType.AppointmentExtended,
  EventType.AppointmentReinstated,
  EventType.AppointmentOverrunning,
  EventType.AppointmentReassigned,
]);

/** Longest a burst may hold back its refetch: under a steady stream of events the view still updates. */
export const COALESCE_MAX_WAIT_MS = 1000;

/**
 * Coalesces bursts of calls into one trailing call after `ms` of quiet, so a flurry of events
 * (stress test, busy morning) triggers one refetch instead of one per event — but never later than
 * `maxWaitMs` after the first call of the burst, so a sustained stream cannot starve the view.
 */
export function coalesce(
  fn: () => void,
  ms: number,
  maxWaitMs = COALESCE_MAX_WAIT_MS,
): { trigger: () => void; cancel: () => void } {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let firstAt: number | undefined;
  return {
    trigger() {
      const now = Date.now();
      if (firstAt === undefined) firstAt = now;
      if (timer !== undefined) clearTimeout(timer);
      const wait = Math.max(0, Math.min(ms, firstAt + maxWaitMs - now));
      timer = setTimeout(() => {
        timer = undefined;
        firstAt = undefined;
        fn();
      }, wait);
    },
    cancel() {
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
      firstAt = undefined;
    },
  };
}

/**
 * Whether a live notice should replace what the client holds. Delivery is at-least-once and can
 * be reordered across instances, so older or duplicate versions are ignored. Reschedules and
 * reassignments keep the id and bump the version, so they need no special case.
 */
export function noticeAffects(notice: AppointmentNotice, current: Appointment | null): boolean {
  if (!current) return false;
  return notice.id === current.id && notice.version > current.version;
}

/**
 * What the appointment panel shows after re-reading `fetched`: it, when the panel still shows the
 * same appointment at that version or older; otherwise what it shows (another appointment opened
 * meanwhile, the panel was closed, or a newer version already arrived).
 */
export function adoptFetched(shown: Appointment | null, fetched: Appointment): Appointment | null {
  return shown && shown.id === fetched.id && fetched.version >= shown.version ? fetched : shown;
}

/**
 * How much of the booking view a live notice makes stale: nothing, the first-available list
 * (which spans days), or that list and the day grid.
 */
export type ViewRefresh = "none" | "next" | "all";

/**
 * What the booking form is showing: the dealership and job lines availability depends on, and the
 * grid's day in the dealership's zone (absent: any notice may concern it).
 */
export type BookingView = { dealershipId: string; serviceTypeIds: string[]; date?: string; timezone?: string };

/** Whether [startAt, endAt) touches the civil `day` in `timeZone` (unknown day or zone: yes). */
export function windowOnDay(startAt: string, endAt: string | undefined, day: string | undefined, timeZone: string | undefined): boolean {
  if (!day || !timeZone) return true;
  const first = formatDate(startAt, timeZone);
  const endMs = endAt ? Date.parse(endAt) - 1 : Date.parse(startAt);
  const last = Number.isFinite(endMs) ? formatDate(new Date(Math.max(endMs, Date.parse(startAt))).toISOString(), timeZone) : first;
  return first <= day && day <= last;
}

/**
 * An AVAILABILITY_CHANGED for another dealership changes nothing in view. One with a window
 * (someone's booking) may change the first-available list, and the grid only when the window is
 * on the grid's day; one without (a technician's roster or skills changed) may move any day, so
 * both are re-read. (A customer's notice carries only the new window: a booking moved away from
 * the grid's day frees its old time unseen until the next refresh of that day — a stale "taken",
 * never a stale "free".)
 */
export function availabilityRefresh(notice: AvailabilityNotice, view: BookingView): ViewRefresh {
  if (view.dealershipId && notice.dealershipId !== view.dealershipId) return "none";
  if (!notice.startAt) return "all";
  return windowOnDay(notice.startAt, notice.endAt, view.date, view.timezone) ? "all" : "next";
}

/**
 * An APPOINTMENT_* notice (staff, or the owning customer): another dealership's changes nothing in
 * view; one on the grid's day re-reads the grid and the first-available list, any other only the
 * list. A reschedule may have left the grid's day (the notice has only the new window): it always
 * re-reads both.
 */
export function appointmentRefresh(type: EventTypeCode, notice: AppointmentNotice, view: BookingView): ViewRefresh {
  if (view.dealershipId && notice.dealershipId !== view.dealershipId) return "none";
  if (type === EventType.AppointmentRescheduled) return "all";
  return windowOnDay(notice.startAt, notice.endAt, view.date, view.timezone) ? "all" : "next";
}

/** The staff day board on screen: its dealership and day, and the appointments it shows. */
export type BoardView = { dealershipId: string; date: string; timezone?: string; appointmentIds: ReadonlySet<string> };

/**
 * Whether an APPOINTMENT_* notice changes the day board: it is the board's dealership and either
 * on the board's day or an appointment the board shows (moved or extended off it).
 */
export function boardAffected(notice: AppointmentNotice, board: BoardView): boolean {
  if (board.dealershipId && notice.dealershipId !== board.dealershipId) return false;
  return board.appointmentIds.has(notice.id) || windowOnDay(notice.startAt, notice.endAt, board.date, board.timezone);
}

/**
 * Whether an APPOINTMENT_* notice may change the appointment lists. A customer only receives their
 * own bookings' notices (any dealership): always. Staff lists show the form's dealership (and the
 * open panel's): a group admin subscribed to every dealership while viewing another one's
 * appointment skips the rest of the group's traffic. (A walk-in customer's bookings at a third
 * dealership are not refreshed live then; they are on the next load of the list.)
 */
export function listsAffected(notice: AppointmentNotice, staffDealershipIds: readonly (string | undefined)[] | null): boolean {
  if (!staffDealershipIds) return true;
  const watched = staffDealershipIds.filter((id): id is string => Boolean(id));
  return watched.length === 0 || watched.includes(notice.dealershipId);
}

/** Whether a CONFIG_UPDATED changes the catalog (dealerships, service types); technicians are not in it. */
export function catalogAffected(notice: ConfigNotice): boolean {
  return notice.entity === AggregateType.Dealership || notice.entity === AggregateType.ServiceType;
}

/**
 * A CONFIG_UPDATED that changes what is bookable in view: the dealership's hours, closures, or
 * policy; a picked service's duration or requirement; a technician (the hub sends those only for
 * the subscribed dealership, which follows the form).
 */
export function configRefresh(notice: ConfigNotice, view: BookingView): ViewRefresh {
  switch (notice.entity) {
    case AggregateType.Dealership:
      return notice.id === view.dealershipId ? "all" : "none";
    case AggregateType.ServiceType:
      return view.serviceTypeIds.includes(notice.id) ? "all" : "none";
    case AggregateType.Technician:
      return "all";
    default:
      return "none";
  }
}

/**
 * The dealership the live socket subscribes to. The hub filters by one dealership at a time ("" =
 * all, which only group admins may use). A group admin viewing another dealership's appointment
 * subscribes to all while the panel is open, so both the panel and the form's dealership stay
 * live (the app drops notices for neither); everyone else follows the form.
 */
export function liveSubscription(formDealershipId: string, panelDealershipId: string | undefined, groupAdmin: boolean): string {
  if (groupAdmin && panelDealershipId && panelDealershipId !== formDealershipId) return "";
  return formDealershipId;
}

/** Backoff for reconnects: 1s, 2s, 4s … capped at 60s. */
export function reconnectDelay(attempt: number): number {
  return Math.min(1000 * 2 ** attempt, 60_000);
}

/**
 * WebSocket close code the hub uses when the session no longer matches the user: signed out, or
 * the periodic re-check found the role, home dealership, or customer link changed ("session
 * revoked"). The app re-reads the user (api.me) before trusting what it shows.
 */
export const CLOSE_POLICY_VIOLATION = 1008;

/** WebSocket close code the hub uses when this user already holds too many sockets. */
export const CLOSE_TRY_AGAIN_LATER = 1013;
/** Minimum wait after the server said "try again later" (close 1013 or HTTP 429). */
export const LONG_BACKOFF_MS = 30_000;

/** Why the last attempt ended, as far as the browser can tell. */
export type ReconnectCause = {
  /** Close code of the socket (1006 when the upgrade itself failed, e.g. an HTTP 429). */
  closeCode?: number;
  /** HTTP status of the ws-ticket request, when that is what failed. */
  status?: number;
  /** Retry-After of that response, in seconds. */
  retryAfter?: number;
};

/**
 * Reconnect backoff for the live socket. The attempt counter resets only when the server's
 * welcome frame ("connected") arrives, never on `onopen`: a socket the hub accepts at the TCP
 * level and then refuses (over the per-user cap) or an upgrade the gateway rejects would
 * otherwise reconnect every second forever. Consecutive failures grow the delay; "try again
 * later" (close 1013, HTTP 429) waits at least LONG_BACKOFF_MS or the server's Retry-After.
 * A little upward jitter keeps tabs that dropped together from reconnecting in lockstep.
 */
export class ReconnectBackoff {
  private failures = 0;

  constructor(private readonly random: () => number = Math.random) {}

  /** The welcome frame arrived: the connection is genuinely up. */
  connected(): void {
    this.failures = 0;
  }

  /** Consecutive failed attempts since the last welcome frame. */
  get attempts(): number {
    return this.failures;
  }

  /** Records a failed or closed connection and returns how long to wait before the next one. */
  next(cause: ReconnectCause = {}): number {
    let delay = reconnectDelay(this.failures);
    this.failures += 1;
    if (cause.closeCode === CLOSE_TRY_AGAIN_LATER || cause.status === 429) {
      delay = Math.max(delay, LONG_BACKOFF_MS, (cause.retryAfter ?? 0) * 1000);
    }
    return Math.round(delay * (1 + 0.2 * this.random()));
  }
}

export function wsUrl(location: Pick<Location, "protocol" | "host">, ticket: string, dealershipId?: string): string {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const params = new URLSearchParams({ ticket });
  if (dealershipId) params.set("dealershipId", dealershipId);
  return `${proto}://${location.host}/api/v1/ws/dispatch?${params}`;
}
