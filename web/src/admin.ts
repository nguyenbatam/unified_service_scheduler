import { isApiError } from "./api";
import { errorText } from "./errors";
import { Role, type AuthUser, type Dealership, type DealershipCalendar, type WeekdayHours } from "./types";

/** A group admin has no home dealership: they may edit the shared catalog and open dealerships. */
export function isGroupAdmin(user: Pick<AuthUser, "role" | "dealershipId"> | null | undefined): boolean {
  return user?.role === Role.Admin && !user.dealershipId;
}

/** Dealerships an admin may configure: all for a group admin, only their own otherwise. */
export function adminDealerships(
  user: Pick<AuthUser, "role" | "dealershipId"> | null | undefined,
  dealerships: Dealership[],
): Dealership[] {
  if (!user || user.role !== Role.Admin) return [];
  if (!user.dealershipId) return dealerships;
  return dealerships.filter((d) => d.id === user.dealershipId);
}

export type AdminTab = "settings" | "calendar" | "technicians" | "capacity" | "catalog" | "outbox";

/** Admin panel tabs: the shared catalog and the outbox are for group admins only. */
export function adminTabs(group: boolean): AdminTab[] {
  const tabs: AdminTab[] = ["settings", "calendar", "technicians", "capacity"];
  return group ? [...tabs, "catalog", "outbox"] : tabs;
}

/**
 * adminErrorText turns an admin save failure into a sentence for the banner. The server's 409
 * messages already say what blocks the change ("3 upcoming appointment(s) fall outside the new
 * hours…"), so they are kept and given a lead-in; the rest get a plain explanation.
 */
export function adminErrorText(err: unknown): string {
  if (!isApiError(err)) return errorText(err);
  switch (err.status) {
    case 400:
      return `Please check the values: ${err.message}`;
    case 403:
      return "You don't have permission to change this (dealership admins can only edit their own dealership; the service catalog and new dealerships need a group admin).";
    case 404:
      return "That item no longer exists — reload the page.";
    case 409:
      return `Not saved: ${err.message}`;
    case 412:
      return "Someone else changed this just now — the latest values are shown. Please review and save again.";
    default:
      return errorText(err);
  }
}

const HHMM = /^\d{2}:\d{2}$/;

/** Minutes after midnight of "HH:MM", or NaN. */
function clockMinutes(v: string): number {
  if (!HHMM.test(v)) return Number.NaN;
  const [h, m] = v.split(":").map(Number);
  return h < 24 && m < 60 ? h * 60 + m : Number.NaN;
}

/**
 * hoursProblem validates opening hours and an optional lunch before a save, so the admin gets a
 * clear message instead of a 400; "" when they are fine. Lunch is given as both times or neither.
 */
export function hoursProblem(open: string, close: string, lunchStart = "", lunchEnd = ""): string {
  const o = clockMinutes(open);
  const c = clockMinutes(close);
  if (Number.isNaN(o) || Number.isNaN(c)) return "Opening and closing times are required.";
  if (o >= c) return "Closing time must be after opening time.";
  if (!lunchStart && !lunchEnd) return "";
  const ls = clockMinutes(lunchStart);
  const le = clockMinutes(lunchEnd);
  if (Number.isNaN(ls) || Number.isNaN(le)) return "Set both lunch start and lunch end, or untick lunch.";
  if (ls >= le) return "Lunch must end after it starts.";
  if (ls < o || le > c) return "Lunch must sit inside the opening hours.";
  return "";
}

export const WEEKDAY_NAMES = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"];

/** One weekday in the calendar editor: dealership defaults, other hours, or closed. */
export type WeekRow = {
  weekday: number;
  mode: "default" | "custom" | "closed";
  openTime: string;
  closeTime: string;
  lunch: boolean;
  lunchStart: string;
  lunchEnd: string;
};

/** weekRows lays the calendar's overrides over the dealership defaults, Monday first. */
export function weekRows(cal: DealershipCalendar | null, d: Dealership | undefined): WeekRow[] {
  return WEEKDAY_NAMES.map((_, i) => {
    const weekday = i + 1;
    const o = cal?.weekly.find((w) => w.weekday === weekday);
    const lunchStart = o ? (o.lunchStart ?? "") : (d?.lunchStart ?? "");
    const lunchEnd = o ? (o.lunchEnd ?? "") : (d?.lunchEnd ?? "");
    return {
      weekday,
      mode: !o ? "default" : o.closed ? "closed" : "custom",
      openTime: o?.openTime ?? d?.openTime ?? "08:00",
      closeTime: o?.closeTime ?? d?.closeTime ?? "17:00",
      lunch: Boolean(lunchStart && lunchEnd),
      lunchStart: lunchStart || "12:00",
      lunchEnd: lunchEnd || "13:00",
    };
  });
}

/** toWeekly is the PUT body for the rows: default days are left out, closed days carry no times. */
export function toWeekly(rows: WeekRow[]): WeekdayHours[] {
  return rows.flatMap((r): WeekdayHours[] => {
    if (r.mode === "default") return [];
    if (r.mode === "closed") return [{ weekday: r.weekday, closed: true }];
    return [
      {
        weekday: r.weekday,
        closed: false,
        openTime: r.openTime,
        closeTime: r.closeTime,
        ...(r.lunch ? { lunchStart: r.lunchStart, lunchEnd: r.lunchEnd } : {}),
      },
    ];
  });
}

/** weekProblem is the first invalid custom weekday, as a sentence; "" when all are fine. */
export function weekProblem(rows: WeekRow[]): string {
  for (const r of rows) {
    if (r.mode !== "custom") continue;
    const p = hoursProblem(r.openTime, r.closeTime, r.lunch ? r.lunchStart : "", r.lunch ? r.lunchEnd : "");
    if (p) return `${WEEKDAY_NAMES[r.weekday - 1]}: ${p}`;
  }
  return "";
}

/** Defaults-and-rules form of one dealership. */
export type DealerHoursForm = {
  openTime: string;
  closeTime: string;
  lunch: boolean;
  lunchStart: string;
  lunchEnd: string;
};

export function dealerHoursForm(d: Dealership): DealerHoursForm {
  return {
    openTime: d.openTime,
    closeTime: d.closeTime,
    lunch: Boolean(d.lunchStart && d.lunchEnd),
    lunchStart: d.lunchStart ?? "12:00",
    lunchEnd: d.lunchEnd ?? "13:00",
  };
}

/**
 * hoursPatch is the part of a dealership PATCH that changes its default hours: only what differs
 * from d (an unchanged field is left out, so it never trips the stranded-booking check), and
 * lunch removal as both "" (the API's way to clear it).
 */
export function hoursPatch(
  d: Dealership,
  f: DealerHoursForm,
): { openTime?: string; closeTime?: string; lunchStart?: string; lunchEnd?: string } {
  const out: { openTime?: string; closeTime?: string; lunchStart?: string; lunchEnd?: string } = {};
  if (f.openTime !== d.openTime) out.openTime = f.openTime;
  if (f.closeTime !== d.closeTime) out.closeTime = f.closeTime;
  const ls = f.lunch ? f.lunchStart : "";
  const le = f.lunch ? f.lunchEnd : "";
  if (ls !== (d.lunchStart ?? "") || le !== (d.lunchEnd ?? "")) {
    out.lunchStart = ls;
    out.lunchEnd = le;
  }
  return out;
}

/** A walk-in customer as the API accepts it ("" = valid): a name and a phone or an email. */
export function customerProblem(c: { name: string; phone: string; email: string }): string {
  const name = c.name.trim();
  const phone = c.phone.trim();
  const email = c.email.trim();
  if (!name) return "Enter the customer's name.";
  if (name.length > 120) return "A name has at most 120 characters.";
  if (!phone && !email) return "Enter a phone number or an email, for confirmations and reminders.";
  if (email && !/^[^\s@]+@[^\s@]+\.[^\s@]*[^\s@.]$/.test(email)) return "That email address doesn't look right.";
  if (phone) {
    if (!/^[\d\s+().-]{6,30}$/.test(phone)) return "A phone number uses digits, spaces and + - ( ) . (6–30 characters).";
    if ((phone.match(/\d/g) ?? []).length < 6) return "A phone number has at least 6 digits.";
  }
  return "";
}

/** Skills in before that after no longer has. */
export function removedSkills<T>(before: readonly T[], after: readonly T[]): T[] {
  return before.filter((s) => !after.includes(s));
}

/** Same skill set, order ignored. */
export function sameSkills<T>(a: readonly T[], b: readonly T[]): boolean {
  return a.length === b.length && a.every((s) => b.includes(s));
}
