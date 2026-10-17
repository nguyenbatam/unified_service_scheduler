import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EventType, AggregateType } from "./types";
import {
  CLOSE_TRY_AGAIN_LATER,
  LIVE_APPT_EVENTS,
  LONG_BACKOFF_MS,
  ReconnectBackoff,
  adoptFetched,
  appointmentRefresh,
  availabilityRefresh,
  boardAffected,
  catalogAffected,
  coalesce,
  configRefresh,
  listsAffected,
  noticeAffects,
  reconnectDelay,
  wsUrl,
} from "./live";
import type { Appointment, AppointmentNotice, ConfigNotice } from "./types";

const appt = { id: "a1", version: 3 } as Appointment;
const notice = (over: Partial<AppointmentNotice>): AppointmentNotice => ({
  id: "a1",
  dealershipId: "d1",
  status: 1,
  version: 4,
  startAt: "2026-10-17T09:00:00Z",
  endAt: "2026-10-17T09:30:00Z",
  ...over,
});

describe("noticeAffects", () => {
  it("accepts a newer version of the current appointment", () => {
    expect(noticeAffects(notice({ version: 4 }), appt)).toBe(true);
  });
  it("ignores stale or duplicate versions (at-least-once, reordered)", () => {
    expect(noticeAffects(notice({ version: 3 }), appt)).toBe(false);
    expect(noticeAffects(notice({ version: 2 }), appt)).toBe(false);
  });
  it("treats an in-place reschedule as a newer version of the same id", () => {
    const moved = notice({ version: 4, startAt: "2026-10-17T11:00:00Z", endAt: "2026-10-17T11:30:00Z" });
    expect(noticeAffects(moved, appt)).toBe(true);
    expect(noticeAffects({ ...moved, id: "a2", version: 1 }, appt)).toBe(false);
  });
  it("ignores other appointments and empty state", () => {
    expect(noticeAffects(notice({ id: "zz" }), appt)).toBe(false);
    expect(noticeAffects(notice({}), null)).toBe(false);
  });
  it("refetches on reassignment events", () => {
    expect(LIVE_APPT_EVENTS.has(EventType.AppointmentReassigned)).toBe(true);
  });
});

describe("coalesce", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("runs once after a burst", () => {
    const fn = vi.fn();
    const c = coalesce(fn, 250);
    c.trigger();
    c.trigger();
    vi.advanceTimersByTime(200);
    c.trigger();
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(250);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("never holds a sustained stream back longer than maxWait", () => {
    const fn = vi.fn();
    const c = coalesce(fn, 250, 1000);
    // An event every 100 ms: a plain trailing debounce would never fire.
    for (let i = 0; i < 10; i++) {
      c.trigger();
      vi.advanceTimersByTime(100);
    }
    expect(fn).toHaveBeenCalledTimes(1);
    for (let i = 0; i < 10; i++) {
      c.trigger();
      vi.advanceTimersByTime(100);
    }
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("cancel drops the pending call", () => {
    const fn = vi.fn();
    const c = coalesce(fn, 100);
    c.trigger();
    c.cancel();
    vi.advanceTimersByTime(200);
    expect(fn).not.toHaveBeenCalled();
  });
});

describe("ws helpers", () => {
  it("backs off and caps", () => {
    expect([0, 1, 2, 10].map(reconnectDelay)).toEqual([1000, 2000, 4000, 60000]);
  });
  it("builds a ticket URL without the access token", () => {
    expect(wsUrl({ protocol: "https:", host: "x.test" }, "t k", "d1")).toBe(
      "wss://x.test/api/v1/ws/dispatch?ticket=t+k&dealershipId=d1",
    );
    expect(wsUrl({ protocol: "http:", host: "x.test" }, "t")).toBe("ws://x.test/api/v1/ws/dispatch?ticket=t");
  });
});

describe("ReconnectBackoff", () => {
  const noJitter = () => 0;

  it("grows across repeated immediate failures (1006, no welcome frame)", () => {
    const b = new ReconnectBackoff(noJitter);
    const delays = Array.from({ length: 8 }, () => b.next({ closeCode: 1006 }));
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000]);
    expect(b.attempts).toBe(8);
  });

  it("resets only when the welcome frame arrives", () => {
    const b = new ReconnectBackoff(noJitter);
    b.next();
    b.next();
    expect(b.next()).toBe(4000);
    b.connected();
    expect(b.attempts).toBe(0);
    expect(b.next({ closeCode: 1001 })).toBe(1000);
  });

  it("waits long after close 1013 (over the per-user socket cap)", () => {
    const b = new ReconnectBackoff(noJitter);
    expect(b.next({ closeCode: CLOSE_TRY_AGAIN_LATER })).toBe(LONG_BACKOFF_MS);
    expect(b.next({ closeCode: CLOSE_TRY_AGAIN_LATER })).toBe(LONG_BACKOFF_MS);
    // Still growing underneath, so a long run of refusals ends up at the cap.
    for (let i = 0; i < 4; i++) b.next({ closeCode: CLOSE_TRY_AGAIN_LATER });
    expect(b.next({ closeCode: CLOSE_TRY_AGAIN_LATER })).toBe(60000);
  });

  it("honours HTTP 429 and a longer Retry-After", () => {
    const b = new ReconnectBackoff(noJitter);
    expect(b.next({ status: 429 })).toBe(LONG_BACKOFF_MS);
    expect(b.next({ status: 429, retryAfter: 90 })).toBe(90000);
    expect(b.next({ status: 503 })).toBe(4000);
  });

  it("adds at most 20% upward jitter and never shortens the long backoff", () => {
    const max = new ReconnectBackoff(() => 1);
    expect(max.next({ closeCode: CLOSE_TRY_AGAIN_LATER })).toBe(36000);
    const min = new ReconnectBackoff(() => 0);
    expect(min.next({ status: 429 })).toBeGreaterThanOrEqual(LONG_BACKOFF_MS);
  });
});

describe("booking view refresh", () => {
  const view = { dealershipId: "d1", serviceTypeIds: ["s1", "s2"] };
  const config = (entity: ConfigNotice["entity"], id: string): ConfigNotice => ({ entity, id, version: 2 });

  it("re-reads the grid and the list for a booking on the grid's day, the list for another day, everything for a window-less notice", () => {
    const onDay = { ...view, date: "2026-10-19", timezone: "Europe/London" };
    expect(availabilityRefresh({ dealershipId: "d1", startAt: "2026-10-19T09:00:00Z", endAt: "2026-10-19T10:00:00Z" }, onDay)).toBe("all");
    expect(availabilityRefresh({ dealershipId: "d1", startAt: "2026-10-20T09:00:00Z", endAt: "2026-10-20T10:00:00Z" }, onDay)).toBe("next");
    // 23:30 UTC on the 18th is already the 19th in London (BST).
    expect(availabilityRefresh({ dealershipId: "d1", startAt: "2026-10-18T23:30:00Z", endAt: "2026-10-19T00:30:00Z" }, onDay)).toBe("all");
    expect(availabilityRefresh({ dealershipId: "d1", startAt: "2026-10-20T09:00:00Z" }, view)).toBe("all"); // no day in view
    expect(availabilityRefresh({ dealershipId: "d1" }, onDay)).toBe("all");
    expect(availabilityRefresh({ dealershipId: "d2" }, view)).toBe("none");
    expect(availabilityRefresh({ dealershipId: "d2" }, { dealershipId: "", serviceTypeIds: [] })).toBe("all");
  });

  it("re-reads the view for config that changes what it can book", () => {
    expect(configRefresh(config(AggregateType.Dealership, "d1"), view)).toBe("all");
    expect(configRefresh(config(AggregateType.Dealership, "d2"), view)).toBe("none");
    expect(configRefresh(config(AggregateType.ServiceType, "s2"), view)).toBe("all");
    expect(configRefresh(config(AggregateType.ServiceType, "s9"), view)).toBe("none");
    expect(configRefresh(config(AggregateType.Technician, "t1"), view)).toBe("all");
  });
});

describe("adoptFetched", () => {
  const shown = { id: "a1", version: 3 } as Appointment;
  it("shows a re-read of the same appointment at the same or a newer version", () => {
    expect(adoptFetched(shown, { id: "a1", version: 4 } as Appointment)?.version).toBe(4);
    expect(adoptFetched(shown, { id: "a1", version: 3 } as Appointment)?.version).toBe(3);
  });
  it("keeps the panel when it moved on: another appointment, closed, or already newer", () => {
    expect(adoptFetched({ id: "a2", version: 1 } as Appointment, { id: "a1", version: 9 } as Appointment)?.id).toBe("a2");
    expect(adoptFetched(null, { id: "a1", version: 9 } as Appointment)).toBeNull();
    expect(adoptFetched(shown, { id: "a1", version: 2 } as Appointment)).toBe(shown);
  });
});

describe("refreshes filtered by day", () => {
  const view = { dealershipId: "d1", serviceTypeIds: ["s1"], date: "2026-10-17", timezone: "Europe/London" };

  it("re-reads the grid only for appointments on its day, the list for any, nothing for other dealerships", () => {
    expect(appointmentRefresh(EventType.AppointmentCreated, notice({}), view)).toBe("all");
    const otherDay = notice({ startAt: "2026-10-18T09:00:00Z", endAt: "2026-10-18T10:00:00Z" });
    expect(appointmentRefresh(EventType.AppointmentCancelled, otherDay, view)).toBe("next");
    expect(appointmentRefresh(EventType.AppointmentCreated, notice({ dealershipId: "d2" }), view)).toBe("none");
  });

  it("re-reads the grid for a reschedule wherever it went (it may have left the grid's day)", () => {
    const moved = notice({ startAt: "2026-10-20T09:00:00Z", endAt: "2026-10-20T10:00:00Z" });
    expect(appointmentRefresh(EventType.AppointmentRescheduled, moved, view)).toBe("all");
  });

  it("re-reads the board for its day, or for an appointment it shows that moved off it", () => {
    const board = { dealershipId: "d1", date: "2026-10-17", timezone: "Europe/London", appointmentIds: new Set(["a9"]) };
    expect(boardAffected(notice({}), board)).toBe(true);
    const elsewhere = notice({ startAt: "2026-10-18T09:00:00Z", endAt: "2026-10-18T10:00:00Z" });
    expect(boardAffected(elsewhere, board)).toBe(false);
    expect(boardAffected({ ...elsewhere, id: "a9" }, board)).toBe(true);
    expect(boardAffected(notice({ dealershipId: "d2" }), board)).toBe(false);
  });
});

describe("list and catalog reload filters", () => {
  it("reloads staff lists only for the form's or the panel's dealership; a customer's always", () => {
    expect(listsAffected(notice({ dealershipId: "d1" }), ["d1", undefined])).toBe(true);
    expect(listsAffected(notice({ dealershipId: "d2" }), ["d1", "d2"])).toBe(true);
    expect(listsAffected(notice({ dealershipId: "d3" }), ["d1", "d2"])).toBe(false);
    expect(listsAffected(notice({ dealershipId: "d3" }), null)).toBe(true);
  });

  it("reloads the catalog for dealerships and service types, never for technicians", () => {
    expect(catalogAffected({ entity: AggregateType.Dealership, id: "d1", version: 2 })).toBe(true);
    expect(catalogAffected({ entity: AggregateType.ServiceType, id: "s1", version: 2 })).toBe(true);
    expect(catalogAffected({ entity: AggregateType.Technician, id: "t1", version: 2 })).toBe(false);
  });
});
