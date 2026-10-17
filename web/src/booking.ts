import { formatClock, formatDate, formatDayLabel, shiftDay, zoneAbbr, zonedToIso } from "./time";
import {
  Status,
  isStaffRole,
  type Appointment,
  type AuthUser,
  type Dealership,
  type DealershipPolicy,
  type ServiceType,
  type Vehicle,
} from "./types";

/**
 * Whether a customer may still cancel or reschedule online: the server refuses changes later than
 * `customerChangeCutoffMinutes` before the start (staff are never bound by it).
 */
export function customerCanChange(appointment: Appointment, policy: DealershipPolicy | undefined, nowMs = Date.now()): boolean {
  if (!policy) return true;
  return nowMs < Date.parse(appointment.startAt) - policy.customerChangeCutoffMinutes * 60_000;
}

/** Max job lines per appointment (backend limit). */
export const MAX_JOB_LINES = 5;

/** Adds or removes one job line, keeping pick order and the 1–MAX_JOB_LINES bounds. */
export function toggleJobLine(ids: string[], id: string): string[] {
  if (jobLineBlock(ids, id)) return ids;
  return ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id];
}

/**
 * Why the job line `id` cannot be toggled ("" = it can): the only line cannot be dropped (a visit
 * needs one service), and no line can be added past MAX_JOB_LINES. Shown on the checkbox.
 */
export function jobLineBlock(ids: string[], id: string): string {
  if (ids.includes(id)) return ids.length > 1 ? "" : "At least one service is needed — pick another one first to swap.";
  return ids.length < MAX_JOB_LINES ? "" : `Up to ${MAX_JOB_LINES} services per visit — remove one first.`;
}

/**
 * One click on service `id`: with a single line (and "add another service" not chosen) it swaps
 * the service, radio-like; with several lines, or once the user chose to add another, it toggles
 * the line (toggleJobLine's bounds hold).
 */
export function pickJobLine(ids: string[], id: string, multi: boolean): string[] {
  if (multi || ids.length > 1) return toggleJobLine(ids, id);
  return [id];
}

/** Same job lines in the same order (line order decides the primary service). */
export function sameJobLines(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((id, i) => id === b[i]);
}

/**
 * The appointment whose own hold availability should ignore: the focused appointment when the grid
 * is computed for its dealership, car, and job lines and it can still be rescheduled (confirmed).
 */
export function rescheduleExclusion(
  appointment: Appointment | null,
  grid: { dealershipId: string; vehicleId: string; serviceTypeIds: string[] },
): string | undefined {
  if (!appointment || appointment.status !== Status.Confirmed) return undefined;
  const lines =
    appointment.lines.length > 0 ? appointment.lines.map((l) => l.serviceTypeId) : [appointment.serviceTypeId];
  return appointment.dealershipId === grid.dealershipId &&
    appointment.vehicleId === grid.vehicleId &&
    sameJobLines(lines, grid.serviceTypeIds)
    ? appointment.id
    : undefined;
}

/**
 * Dealerships the user can book and see the day board of: staff of one dealership get 403 on any
 * other's, so only their own is offered; group admins and customers see them all.
 */
export function bookableDealerships(
  user: Pick<AuthUser, "role" | "dealershipId"> | null | undefined,
  dealerships: Dealership[],
): Dealership[] {
  if (!user || !isStaffRole(user.role) || !user.dealershipId) return dealerships;
  return dealerships.filter((d) => d.id === user.dealershipId);
}

/** Workshop actions whose timing the server checks (lifecycle.go). */
export type TimedAction = "checkIn" | "noShow" | "reinstate";

/**
 * Why a workshop action would be refused right now ("" = allowed), mirroring the server's timing
 * rules so the button can be disabled with the reason instead of failing with a 422: check-in
 * opens on the appointment's local day and closes when the booked window ends, a no-show only once
 * the booked start has passed, and a reinstate only while the booked window lasts.
 */
export function actionBlock(kind: TimedAction, a: Appointment, timeZone: string, nowMs = Date.now()): string {
  const now = new Date(nowMs).toISOString();
  switch (kind) {
    case "checkIn": {
      const day = formatDate(a.startAt, timeZone);
      if (formatDate(now, timeZone) < day) return `Check-in opens on the appointment day (${formatDayLabel(day)}).`;
      return nowMs >= Date.parse(a.endAt) ? "The booked window has ended — reschedule the appointment instead." : "";
    }
    case "noShow":
      return nowMs < Date.parse(a.startAt)
        ? `A no-show can be marked once the booked start (${formatClock(a.startAt, timeZone)}) has passed.`
        : "";
    case "reinstate":
      return nowMs >= Date.parse(a.endAt) ? "The booked window has ended — book a new appointment instead." : "";
  }
}

/**
 * The instants at which a timed action's verdict can change (actionBlock): check-in opens at the
 * appointment day's local midnight, a no-show at the booked start, a reinstate closes at the
 * booked end. The panel re-renders exactly then instead of waiting for its minute tick.
 */
export function actionBoundaries(a: Appointment, timeZone: string): number[] {
  return [
    Date.parse(zonedToIso(`${formatDate(a.startAt, timeZone)}T00:00`, timeZone)),
    Date.parse(a.startAt),
    Date.parse(a.endAt),
  ];
}

/**
 * The dealership's today by this browser's clock, never before the server's `today`: that one is
 * read with the catalog and goes stale when a tab stays open overnight.
 */
export function dealershipToday(d: Pick<Dealership, "today" | "timezone">, nowMs = Date.now()): string {
  const local = formatDate(new Date(nowMs).toISOString(), d.timezone);
  return local > d.today ? local : d.today;
}

/** The instant (ms) the dealership's today next moves on: its next local midnight. */
export function nextDealershipMidnight(d: Pick<Dealership, "today" | "timezone">, nowMs = Date.now()): number {
  return Date.parse(zonedToIso(`${shiftDay(dealershipToday(d, nowMs), 1)}T00:00`, d.timezone));
}

/**
 * The first day a customer can book online: today, or tomorrow when the lead time already reaches
 * past today's closing time (the whole of today would show "Too soon").
 */
export function firstBookableDay(d: Dealership, nowMs = Date.now()): string {
  const earliest = new Date(nowMs + d.policy.customerLeadMinutes * 60_000).toISOString();
  const day = formatDate(earliest, d.timezone);
  const clock = formatClock(earliest, d.timezone);
  const today = dealershipToday(d, nowMs);
  const first = day > today ? day : today;
  return clock >= d.closeTime && first === day ? shiftDay(day, 1) : first;
}

/** The last day a customer can book online (the booking horizon), for the date field's max. */
export function lastBookableDay(d: Dealership, nowMs = Date.now()): string {
  return shiftDay(dealershipToday(d, nowMs), d.policy.bookingHorizonDays);
}


/** "…3456": the end of a VIN, enough to tell two cars of the same model apart at the desk. */
function vinTail(vin: string): string {
  return vin.length > 6 ? `VIN …${vin.slice(-6)}` : `VIN ${vin}`;
}

/**
 * The one-line review shown next to Confirm: the car (staff also see its VIN end and owner), the
 * job lines in order, the dealership, the start in its zone, and whether the customer waits.
 */
export function bookingReview(o: {
  vehicle: Vehicle | undefined;
  serviceTypeIds: string[];
  services: ServiceType[];
  dealership: Dealership | undefined;
  start: string;
  isWaiter: boolean;
  isStaff: boolean;
}): string {
  const car = o.vehicle
    ? `${o.vehicle.year} ${o.vehicle.make} ${o.vehicle.model}${o.isStaff ? ` (${vinTail(o.vehicle.vin)}, ${o.vehicle.customerName || "no owner"})` : ""}`
    : "the selected car";
  const lines = o.serviceTypeIds.map((id) => o.services.find((s) => s.id === id)?.name ?? "a service").join(" + ");
  const tz = o.dealership?.timezone ?? "UTC";
  const when = `${formatDayLabel(formatDate(o.start, tz))} ${formatClock(o.start, tz)} ${zoneAbbr(tz, o.start)}`;
  const wait = o.isWaiter ? (o.isStaff ? "waiter" : "you wait on site") : o.isStaff ? "drop-off" : "you drop the car off";
  return [car, lines, o.dealership?.name ?? "", when, wait].filter(Boolean).join(" · ");
}
