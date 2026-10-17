import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { dealership, deferred, makeAppointment, makeAvailability, on, pinToday, stubFetch } from "./testFixtures";
import { NEXT_SLOT_TAKEN, useBookingForm } from "./useBookingForm";
import { Status, type Appointment, type Availability } from "./types";

const run = async <T,>(fn: () => Promise<T>) => {
  try {
    return await fn();
  } catch {
    return undefined;
  }
};

function setup(appointment: Appointment | null = null) {
  const hook = renderHook((props: { appointment: Appointment | null }) =>
    // Staff: the form starts on the fixture's today whatever the real clock says.
    useBookingForm({ run, dealerships: [dealership], appointment: props.appointment, isStaff: true }),
    { initialProps: { appointment } },
  );
  act(() => {
    hook.result.current.seed(dealership, "s1");
    hook.result.current.defaultVehicle("v1");
  });
  return hook;
}

beforeEach(() => pinToday());
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("useBookingForm", () => {
  it("drops the grid and the picked slot when the date changes, then re-reads the grid for the new day", async () => {
    const fetch = stubFetch(
      on("GET", "/api/v1/availability", (url) => {
        const day = url.searchParams.get("date") ?? "";
        return { body: makeAvailability(day, [`${day}T09:00:00Z`]) };
      }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    act(() => result.current.selectSlot(result.current.availability!.slots[0]));
    expect(result.current.selectedSlot).not.toBeNull();

    act(() => result.current.changeDate("2026-10-20"));

    expect(result.current.date).toBe("2026-10-20");
    expect(result.current.availability).toBeNull();
    expect(result.current.selectedSlot).toBeNull();
    // A grid was on screen: the new day's is loaded without another click.
    await waitFor(() => expect(result.current.availability?.date).toBe("2026-10-20"));
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("refreshes the grid of the inputs shown now, even when called from an older render", async () => {
    const days: string[] = [];
    stubFetch(
      on("GET", "/api/v1/availability", (url) => {
        const day = url.searchParams.get("date") ?? "";
        days.push(day);
        return { body: makeAvailability(day, [`${day}T09:00:00Z`]) };
      }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    // A booking captured this render, then the user moved to another day while it was in flight.
    const atBooking = result.current;
    act(() => result.current.changeDate("2026-10-20"));
    await waitFor(() => expect(result.current.availability?.date).toBe("2026-10-20"));
    await act(() => atBooking.refreshAvailability({ silent: true, resetSelection: true }));
    expect(days.at(-1)).toBe("2026-10-20");
    expect(result.current.availability?.date).toBe("2026-10-20"); // never the old day's grid
  });

  it("does not load a grid on a date change when none was shown", async () => {
    const fetch = stubFetch(on("GET", "/api/v1/availability", { body: makeAvailability("2026-10-20", []) }));
    const { result } = setup();
    act(() => result.current.changeDate("2026-10-20"));
    await new Promise((r) => setTimeout(r, 20));
    expect(fetch).not.toHaveBeenCalled();
    expect(result.current.availability).toBeNull();
  });

  it("drops a picked slot that a live refresh shows as taken", async () => {
    let taken = false;
    stubFetch(
      on("GET", "/api/v1/availability", () => {
        const grid = makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z", "2026-10-19T10:00:00Z"]);
        if (taken) grid.slots[0] = { ...grid.slots[0], available: false, freeBays: 0, reason: "no_bay" };
        return { body: grid };
      }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    act(() => result.current.selectSlot(result.current.availability!.slots[1]));
    await act(() => result.current.refreshAvailability({ silent: true }));
    expect(result.current.selectedSlot?.start).toBe("2026-10-19T10:00:00Z"); // still offered: kept

    act(() => result.current.selectSlot(result.current.availability!.slots[0]));
    taken = true;
    await act(() => result.current.refreshAvailability({ silent: true }));
    expect(result.current.selectedSlot).toBeNull();
  });

  it("resets every field and drops responses still in flight", async () => {
    const slow = deferred<{ body: Availability }>();
    stubFetch(on("GET", "/api/v1/availability", () => slow.promise));
    const { result } = setup();
    act(() => result.current.changeWaiter(true));
    let pending!: Promise<void>;
    act(() => {
      pending = result.current.loadAvailability();
    });

    act(() => result.current.reset());
    await act(async () => {
      slow.resolve({ body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]) });
      await pending;
    });

    expect(result.current.dealershipId).toBe("");
    expect(result.current.vehicleId).toBe("");
    expect(result.current.serviceTypeIds).toEqual([]);
    expect(result.current.date).toBe("");
    expect(result.current.isWaiter).toBe(false);
    expect(result.current.availability).toBeNull();
    expect(result.current.selectedSlot).toBeNull();
    // The next user's first vehicle page seeds the pick again.
    act(() => result.current.defaultVehicle("v2"));
    expect(result.current.vehicleId).toBe("v2");
  });

  it("ends reschedule mode when the car, services, or dealership change, and when the appointment stops being movable", async () => {
    stubFetch(on("GET", "/api/v1/availability", { body: makeAvailability("2026-10-19", []) }));
    const appt = makeAppointment();
    const { result, rerender } = setup(appt);
    await act(() => result.current.startReschedule(appt, "Europe/London"));
    act(() => result.current.changeVehicle("v9"));
    expect(result.current.rescheduleTarget).toBeNull();

    await act(() => result.current.startReschedule(appt, "Europe/London"));
    act(() => result.current.changeServices(["s2"]));
    expect(result.current.rescheduleTarget).toBeNull();

    await act(() => result.current.startReschedule(appt, "Europe/London"));
    // Changing the day keeps moving the same appointment.
    act(() => result.current.changeDate("2026-10-20"));
    expect(result.current.rescheduleTarget?.id).toBe("a1");
    // Checked in at the desk meanwhile: it can no longer be moved.
    rerender({ appointment: { ...appt, status: Status.CheckedIn, version: 2 } });
    await waitFor(() => expect(result.current.rescheduleTarget).toBeNull());
  });

  it("never lets an older response overwrite the grid of newer inputs", async () => {
    const slow = deferred<{ body: Availability }>();
    stubFetch(
      on("GET", "/api/v1/availability", (url) =>
        url.searchParams.get("date") === "2026-10-19"
          ? slow.promise
          : { body: makeAvailability("2026-10-20", ["2026-10-20T09:00:00Z"]) },
      ),
    );
    const { result } = setup();
    let first!: Promise<void>;
    act(() => {
      first = result.current.loadAvailability();
    });
    act(() => result.current.changeDate("2026-10-20"));
    await act(() => result.current.loadAvailability());
    expect(result.current.availability?.date).toBe("2026-10-20");

    // The 19th answers last: it must be dropped.
    await act(async () => {
      slow.resolve({ body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]) });
      await first;
    });
    expect(result.current.availability?.date).toBe("2026-10-20");
  });

  it("enters reschedule mode only on request and excludes the appointment's own hold", async () => {
    const fetch = stubFetch(on("GET", "/api/v1/availability", { body: makeAvailability("2026-10-19", []) }));
    const appt = makeAppointment();
    const { result } = setup(appt);
    // Viewing alone changes nothing.
    expect(result.current.rescheduleTarget).toBeNull();

    await act(() => result.current.startReschedule(appt, "Europe/London"));

    expect(result.current.rescheduleTarget?.id).toBe("a1");
    expect(result.current.excludeAppointmentId).toBe("a1");
    const url = new URL(String(fetch.mock.calls.at(-1)![0]), "http://localhost");
    expect(url.searchParams.get("excludeAppointmentId")).toBe("a1");
    expect(url.searchParams.get("date")).toBe("2026-10-19");

    act(() => result.current.stopReschedule());
    expect(result.current.rescheduleTarget).toBeNull();
    expect(result.current.availability).toBeNull();
  });

  it("does not load a grid on a live refresh before the user asked for one", async () => {
    const fetch = stubFetch(on("GET", "/api/v1/availability", { body: makeAvailability("2026-10-19", []) }));
    const { result } = setup();
    await act(() => result.current.refreshAvailability({ silent: true }));
    expect(fetch).not.toHaveBeenCalled();
    await act(() => result.current.loadAvailability());
    await act(() => result.current.refreshAvailability({ silent: true }));
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  });

  it("re-reads a grid on screen for new services, waiter flag, or car, and leaves an unshown one alone", async () => {
    const asked: string[] = [];
    stubFetch(
      on("GET", "/api/v1/availability", (url) => {
        asked.push(`${url.searchParams.get("vehicleId")}/${url.searchParams.get("serviceTypeIds") ?? url.searchParams.get("serviceTypeId")}/${url.searchParams.get("isWaiter") ?? "false"}`);
        return { body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]) };
      }),
    );
    const { result } = setup();
    act(() => result.current.changeServices(["s1", "s2"]));
    await new Promise((r) => setTimeout(r, 20));
    expect(asked).toEqual([]); // nothing on screen yet

    await act(() => result.current.loadAvailability());
    act(() => result.current.changeServices(["s2"]));
    expect(result.current.availability).toBeNull(); // the old grid goes at once
    await waitFor(() => expect(result.current.availability).not.toBeNull());
    act(() => result.current.changeWaiter(true));
    await waitFor(() => expect(result.current.availability).not.toBeNull());
    act(() => result.current.changeVehicle("v2"));
    await waitFor(() => expect(asked).toHaveLength(4));
    expect(asked).toEqual(["v1/s1,s2/false", "v1/s2/false", "v1/s2/true", "v2/s2/true"]);
  });
});

describe("useBookingForm silent refresh of the first-available list", () => {
  it("re-reads the list and the grid on request, keeping the pick while it is still offered", async () => {
    let gridCalls = 0;
    let nextCalls = 0;
    let freeAt = ["2026-10-19T09:00:00Z", "2026-10-19T10:00:00Z"];
    stubFetch(
      on("GET", "/api/v1/availability/next", (url) => {
        nextCalls++;
        expect(url.searchParams.get("from")).toBe("2026-10-19");
        return {
          body: {
            timezone: "Europe/London",
            durationMinutes: 60,
            searchedFrom: "2026-10-19",
            searchedTo: "2026-11-02",
            slots: freeAt.map((start) => ({ date: "2026-10-19", start, end: start, freeBays: 1, freeTechnicians: 1 })),
          },
        };
      }),
      on("GET", "/api/v1/availability", () => {
        gridCalls++;
        return { body: makeAvailability("2026-10-19", freeAt) };
      }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    await act(() => result.current.findFirstAvailable());
    act(() => result.current.selectSlot(result.current.availability!.slots[1]));
    expect([gridCalls, nextCalls]).toEqual([1, 1]);

    await act(() => result.current.refreshAvailability({ silent: true }));
    expect([gridCalls, nextCalls]).toEqual([2, 1]); // a booking elsewhere: the grid only

    freeAt = ["2026-10-19T10:00:00Z"]; // a roster change took 09:00 away
    await act(() => result.current.refreshAvailability({ silent: true, next: true }));
    expect([gridCalls, nextCalls]).toEqual([3, 2]);
    expect(result.current.nextSlots?.slots.map((s) => s.start)).toEqual(["2026-10-19T10:00:00Z"]);
    expect(result.current.selectedSlot?.start).toBe("2026-10-19T10:00:00Z"); // still offered: kept

    freeAt = [];
    await act(() => result.current.refreshAvailability({ silent: true, next: true }));
    expect(result.current.nextSlots?.slots).toEqual([]);
    expect(result.current.selectedSlot).toBeNull();
  });

  it("asks customers for a few best fits over several days, staff for the earliest", async () => {
    const prefers: (string | null)[] = [];
    const shapes: string[] = [];
    stubFetch(
      on("GET", "/api/v1/availability/next", (url) => {
        prefers.push(url.searchParams.get("prefer"));
        shapes.push(`${url.searchParams.get("limit")}/${url.searchParams.get("perDay")}`);
        return { body: { timezone: "Europe/London", durationMinutes: 60, searchedFrom: "2026-10-19", searchedTo: "2026-11-02", slots: [] } };
      }),
    );
    const { result } = setup();
    await act(() => result.current.findFirstAvailable());
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-19T07:00:00Z"));
    const customer = renderHook(() => useBookingForm({ run, dealerships: [dealership], appointment: null }));
    act(() => {
      customer.result.current.seed(dealership, "s1");
      customer.result.current.defaultVehicle("v1");
    });
    await act(() => customer.result.current.findFirstAvailable());
    vi.useRealTimers();
    expect(prefers).toEqual([null, "recommended"]);
    expect(shapes).toEqual(["6/3", "4/2"]);
  });

  it("does not search on a silent refresh when no list is shown", async () => {
    const search = vi.fn(() => ({ body: {} }));
    stubFetch(
      on("GET", "/api/v1/availability/next", search),
      on("GET", "/api/v1/availability", { body: makeAvailability("2026-10-19", []) }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    await act(() => result.current.refreshAvailability({ silent: true, next: true }));
    expect(search).not.toHaveBeenCalled();
    expect(result.current.nextSlots).toBeNull();
  });
});

describe("useBookingForm for customers", () => {
  it("starts on the first day the lead time leaves bookable", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    // 16:30 BST on the 19th: with 60 minutes notice nothing is left before 17:00 closing.
    vi.setSystemTime(new Date("2026-10-19T15:30:00Z"));
    const { result } = renderHook(() => useBookingForm({ run, dealerships: [dealership], appointment: null }));
    act(() => result.current.seed(dealership, "s1"));
    expect(result.current.date).toBe("2026-10-20");
    const staff = renderHook(() => useBookingForm({ run, dealerships: [dealership], appointment: null, isStaff: true }));
    act(() => staff.result.current.seed(dealership, "s1"));
    expect(staff.result.current.date).toBe("2026-10-19");
    vi.useRealTimers();
  });

  it("opens the first available day's grid and lists the earliest starts", async () => {
    stubFetch(
      on("GET", "/api/v1/availability/next", {
        body: {
          timezone: "Europe/London",
          durationMinutes: 60,
          searchedFrom: "2026-10-19",
          searchedTo: "2026-11-02",
          slots: [{ date: "2026-10-22", start: "2026-10-22T08:00:00Z", end: "2026-10-22T09:00:00Z", freeBays: 1, freeTechnicians: 1 }],
        },
      }),
      on("GET", "/api/v1/availability", (url) => ({ body: makeAvailability(url.searchParams.get("date") ?? "", []) })),
    );
    const { result } = setup();
    await act(() => result.current.openFirstAvailable());
    expect(result.current.date).toBe("2026-10-22");
    expect(result.current.availability?.date).toBe("2026-10-22");
    expect(result.current.nextSlots?.slots).toHaveLength(1);
    expect(result.current.selectedSlot).toBeNull();
  });
});

describe("useBookingForm first available and booking keys", () => {
  const next = (start: string) => ({
    timezone: "Europe/London",
    durationMinutes: 60,
    searchedFrom: "2026-10-19",
    searchedTo: "2026-11-02",
    slots: [{ date: start.slice(0, 10), start, end: start, freeBays: 1, freeTechnicians: 1 }],
  });

  it("selects a first-available start only when the fresh grid offers it; otherwise says so and re-reads the list", async () => {
    let nextReads = 0;
    stubFetch(
      on("GET", "/api/v1/availability/next", () => {
        nextReads++;
        return { body: next("2026-10-22T08:00:00Z") };
      }),
      on("GET", "/api/v1/availability", (url) => {
        const grid = makeAvailability(url.searchParams.get("date") ?? "", ["2026-10-22T08:00:00Z"]);
        grid.slots[0] = { ...grid.slots[0], available: false, freeBays: 0, reason: "no_bay" };
        return { body: grid };
      }),
    );
    const errors: string[] = [];
    const reporting = async <T,>(fn: () => Promise<T>) => {
      try {
        return await fn();
      } catch (err) {
        errors.push((err as Error).message);
        return undefined;
      }
    };
    const { result } = renderHook(() => useBookingForm({ run: reporting, dealerships: [dealership], appointment: null, isStaff: true }));
    act(() => {
      result.current.seed(dealership, "s1");
      result.current.defaultVehicle("v1");
    });
    await act(() => result.current.findFirstAvailable());
    await act(() => result.current.pickNextSlot(result.current.nextSlots!.slots[0]));
    expect(result.current.selectedSlot).toBeNull();
    expect(errors).toEqual([NEXT_SLOT_TAKEN]);
    await waitFor(() => expect(nextReads).toBe(2));
  });

  it("re-reads only the first-available list when asked for the list alone", async () => {
    const reads = { grid: 0, next: 0 };
    stubFetch(
      on("GET", "/api/v1/availability/next", () => {
        reads.next++;
        return { body: next("2026-10-19T09:00:00Z") };
      }),
      on("GET", "/api/v1/availability", () => {
        reads.grid++;
        return { body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]) };
      }),
    );
    const { result } = setup();
    await act(() => result.current.loadAvailability());
    await act(() => result.current.findFirstAvailable());
    await act(() => result.current.refreshAvailability({ silent: true, next: true, grid: false }));
    expect(reads).toEqual({ grid: 1, next: 2 });
  });

  it("keeps the Idempotency-Key while the request is the same, and renews it once it changed or settled", () => {
    const { result } = setup();
    const request = { dealershipId: "d1", vehicleId: "v1", serviceTypeIds: ["s1"], startAt: "2026-10-19T09:00:00Z", isWaiter: false };
    const first = result.current.bookingKey(request);
    expect(result.current.bookingKey({ ...request })).toBe(first); // a re-click, or a retry after a 503
    const other = result.current.bookingKey({ ...request, startAt: "2026-10-19T10:00:00Z" });
    expect(other).not.toBe(first);
    result.current.settleBookingKey(); // booked, or refused for certain
    expect(result.current.bookingKey({ ...request, startAt: "2026-10-19T10:00:00Z" })).not.toBe(other);
  });
});

describe("useBookingForm start day", () => {
  it("starts staff on the dealership's today by the clock, not the catalog's stale today", () => {
    // 00:30 on the 20th in London; the catalog loaded yesterday still says the 19th.
    vi.setSystemTime(new Date("2026-10-19T23:30:00Z"));
    const { result } = renderHook(() => useBookingForm({ run, dealerships: [dealership], appointment: null, isStaff: true }));
    act(() => result.current.seed(dealership, undefined));
    expect(result.current.date).toBe("2026-10-20");
  });
});
