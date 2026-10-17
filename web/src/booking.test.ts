import { describe, expect, it } from "vitest";
import { availabilityParams } from "./api";
import {
  MAX_JOB_LINES,
  actionBlock,
  bookableDealerships,
  bookingReview,
  customerCanChange,
  dealershipToday,
  firstBookableDay,
  jobLineBlock,
  lastBookableDay,
  nextDealershipMidnight,
  pickJobLine,
  rescheduleExclusion,
  sameJobLines,
  toggleJobLine,
} from "./booking";
import { dealership, makeAppointment, service } from "./testFixtures";
import { Role, Status, type Appointment, type Dealership } from "./types";

describe("job lines", () => {
  it("adds in pick order and removes", () => {
    expect(toggleJobLine(["a"], "b")).toEqual(["a", "b"]);
    expect(toggleJobLine(["a", "b", "c"], "b")).toEqual(["a", "c"]);
  });
  it("keeps at least one line", () => {
    expect(toggleJobLine(["a"], "a")).toEqual(["a"]);
  });
  it("says why a line cannot be toggled, for the disabled checkbox", () => {
    expect(jobLineBlock(["a"], "a")).toMatch(/At least one service/);
    expect(jobLineBlock(["a", "b"], "a")).toBe("");
    expect(jobLineBlock(["a"], "b")).toBe("");
    expect(jobLineBlock(["a", "b", "c", "d", "e"], "f")).toMatch(/Up to 5 services/);
  });
  it("caps the number of lines", () => {
    const full = ["a", "b", "c", "d", "e"];
    expect(full).toHaveLength(MAX_JOB_LINES);
    expect(toggleJobLine(full, "f")).toEqual(full);
  });
  it("compares order-sensitively", () => {
    expect(sameJobLines(["a", "b"], ["a", "b"])).toBe(true);
    expect(sameJobLines(["a", "b"], ["b", "a"])).toBe(false);
    expect(sameJobLines(["a"], ["a", "b"])).toBe(false);
  });
});

describe("reschedule exclusion", () => {
  const appt = {
    id: "appt-1",
    dealershipId: "d1",
    vehicleId: "v1",
    serviceTypeId: "s1",
    status: Status.Confirmed,
    lines: [{ lineNo: 1, serviceTypeId: "s1", code: "OIL", name: "Oil", durationMinutes: 30 }],
  } as Appointment;
  const grid = { dealershipId: "d1", vehicleId: "v1", serviceTypeIds: ["s1"] };

  it("excludes the focused confirmed appointment on its own grid", () => {
    expect(rescheduleExclusion(appt, grid)).toBe("appt-1");
  });
  it("does not exclude for another car, dealership, job, or a non-confirmed status", () => {
    expect(rescheduleExclusion(null, grid)).toBeUndefined();
    expect(rescheduleExclusion(appt, { ...grid, vehicleId: "v2" })).toBeUndefined();
    expect(rescheduleExclusion(appt, { ...grid, dealershipId: "d2" })).toBeUndefined();
    expect(rescheduleExclusion(appt, { ...grid, serviceTypeIds: ["s2"] })).toBeUndefined();
    expect(rescheduleExclusion({ ...appt, status: Status.CheckedIn }, grid)).toBeUndefined();
  });
  it("is sent as excludeAppointmentId only when set", () => {
    const base = { dealershipId: "d1", vehicleId: "v1", serviceTypeIds: ["s1"] };
    expect(availabilityParams({ ...base, excludeAppointmentId: "appt-1" }).get("excludeAppointmentId")).toBe("appt-1");
    expect(availabilityParams(base).has("excludeAppointmentId")).toBe(false);
  });
});

describe("customerCanChange", () => {
  const policy = {
    customerLeadMinutes: 60,
    customerChangeCutoffMinutes: 60,
    bookingHorizonDays: 90,
    noShowGraceMinutes: 30,
    expressMaxMinutes: 60,
  };
  const appt = { startAt: "2026-10-19T10:00:00Z" } as Appointment;

  it("allows changes until the cutoff before the start", () => {
    expect(customerCanChange(appt, policy, Date.parse("2026-10-19T08:59:00Z"))).toBe(true);
    expect(customerCanChange(appt, policy, Date.parse("2026-10-19T09:00:00Z"))).toBe(false);
    expect(customerCanChange(appt, undefined, Date.parse("2026-10-19T09:59:00Z"))).toBe(true);
  });
});

describe("bookableDealerships", () => {
  const north = { id: "d1" } as Dealership;
  const river = { id: "d2" } as Dealership;
  it("limits staff of one dealership to it and leaves everyone else all", () => {
    expect(bookableDealerships({ role: Role.Dispatcher, dealershipId: "d2" }, [north, river])).toEqual([river]);
    expect(bookableDealerships({ role: Role.Admin, dealershipId: "d1" }, [north, river])).toEqual([north]);
    expect(bookableDealerships({ role: Role.Admin }, [north, river])).toEqual([north, river]);
    expect(bookableDealerships({ role: Role.Customer }, [north, river])).toEqual([north, river]);
  });
});

describe("actionBlock", () => {
  // 10:00–11:00 London (BST) on Mon 19 Oct 2026.
  const appt = makeAppointment();
  const at = (iso: string) => Date.parse(iso);

  it("opens check-in on the appointment's local day, early arrival included, until the window ends", () => {
    expect(actionBlock("checkIn", appt, "Europe/London", at("2026-10-18T22:59:00Z"))).toBe(
      "Check-in opens on the appointment day (Mon, 19 Oct 2026).",
    );
    // 00:00 BST on the 19th is still the 18th in UTC.
    expect(actionBlock("checkIn", appt, "Europe/London", at("2026-10-18T23:00:00Z"))).toBe("");
    expect(actionBlock("checkIn", appt, "Europe/London", at("2026-10-19T09:59:00Z"))).toBe("");
    // 11:00 BST: the booked window is over.
    expect(actionBlock("checkIn", appt, "Europe/London", at("2026-10-19T10:00:00Z"))).toBe(
      "The booked window has ended — reschedule the appointment instead.",
    );
  });

  it("allows a no-show only once the booked start has passed", () => {
    expect(actionBlock("noShow", appt, "Europe/London", at("2026-10-19T08:59:00Z"))).toMatch(/booked start \(10:00\)/);
    expect(actionBlock("noShow", appt, "Europe/London", at("2026-10-19T09:00:00Z"))).toBe("");
  });

  it("allows a reinstate only while the booked window lasts", () => {
    expect(actionBlock("reinstate", appt, "Europe/London", at("2026-10-19T09:59:00Z"))).toBe("");
    expect(actionBlock("reinstate", appt, "Europe/London", at("2026-10-19T10:00:00Z"))).toMatch(/window has ended/);
  });
});

describe("bookable days", () => {
  // Northside: 08:00–17:00 London, 60 minutes lead time, 90 days ahead; today 2026-10-19.
  it("starts customers today while the lead time leaves part of it", () => {
    expect(firstBookableDay(dealership, Date.parse("2026-10-19T09:00:00Z"))).toBe("2026-10-19");
  });
  it("starts customers tomorrow once the lead time reaches closing", () => {
    expect(firstBookableDay(dealership, Date.parse("2026-10-19T15:00:00Z"))).toBe("2026-10-20");
    const longLead = { ...dealership, policy: { ...dealership.policy, customerLeadMinutes: 2 * 1440 } };
    expect(firstBookableDay(longLead, Date.parse("2026-10-19T09:00:00Z"))).toBe("2026-10-21");
  });
  it("ends at the booking horizon", () => {
    expect(lastBookableDay(dealership, Date.parse("2026-10-19T09:00:00Z"))).toBe("2027-01-17");
  });
});


describe("one-click service swap", () => {
  it("replaces a single service, toggles once adding another or with several lines", () => {
    expect(pickJobLine(["a"], "b", false)).toEqual(["b"]);
    expect(pickJobLine([], "b", false)).toEqual(["b"]);
    expect(pickJobLine(["a"], "a", false)).toEqual(["a"]);
    expect(pickJobLine(["a"], "b", true)).toEqual(["a", "b"]);
    expect(pickJobLine(["a", "b"], "b", false)).toEqual(["a"]);
  });
});

describe("bookingReview", () => {
  const car = { id: "v1", customerId: "c1", customerName: "Casey", vin: "JT2BG22K1W0123456", make: "Toyota", model: "Corolla", year: 2020 };
  const base = { vehicle: car, serviceTypeIds: ["s1"], services: [service], dealership, start: "2026-10-19T09:00:00Z", isWaiter: false };

  it("names the car with its VIN end and owner for staff", () => {
    expect(bookingReview({ ...base, isStaff: true, isWaiter: true })).toBe(
      "2020 Toyota Corolla (VIN …123456, Casey) · Oil change · Northside · Mon, 19 Oct 2026 10:00 BST · waiter",
    );
  });

  it("speaks to the customer, listing every job line in order", () => {
    const brakes = { ...service, id: "s2", name: "Brake check" };
    expect(bookingReview({ ...base, serviceTypeIds: ["s2", "s1"], services: [service, brakes], isStaff: false })).toBe(
      "2020 Toyota Corolla · Brake check + Oil change · Northside · Mon, 19 Oct 2026 10:00 BST · you drop the car off",
    );
  });
});

describe("dealership today by the clock", () => {
  it("moves on past the catalog's today overnight, never before it", () => {
    // 23:30 UTC on the 19th is 00:30 on the 20th in London (BST).
    expect(dealershipToday(dealership, Date.parse("2026-10-19T23:30:00Z"))).toBe("2026-10-20");
    expect(dealershipToday(dealership, Date.parse("2026-10-19T12:00:00Z"))).toBe("2026-10-19");
    expect(dealershipToday(dealership, Date.parse("2026-10-10T12:00:00Z"))).toBe("2026-10-19");
  });

  it("finds the next local midnight", () => {
    expect(new Date(nextDealershipMidnight(dealership, Date.parse("2026-10-19T12:00:00Z"))).toISOString()).toBe(
      "2026-10-19T23:00:00.000Z",
    );
  });
});
