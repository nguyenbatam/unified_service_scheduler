import { describe, expect, it } from "vitest";
import {
  adminDealerships,
  adminErrorText,
  adminTabs,
  customerProblem,
  dealerHoursForm,
  hoursPatch,
  hoursProblem,
  isGroupAdmin,
  removedSkills,
  sameSkills,
  toWeekly,
  weekProblem,
  weekRows,
} from "./admin";
import { vinProblem } from "./vehicles";
import { Role, type Dealership, type DealershipCalendar } from "./types";

function dealer(id: string, extra: Partial<Dealership> = {}): Dealership {
  return {
    id,
    name: id,
    timezone: "Europe/London",
    openTime: "08:00",
    closeTime: "17:00",
    lunchStart: "12:00",
    lunchEnd: "13:00",
    stagingCapacity: 0,
    stagingWindowMinutes: 60,
    policy: {
      customerLeadMinutes: 60,
      customerChangeCutoffMinutes: 60,
      bookingHorizonDays: 90,
      noShowGraceMinutes: 30,
      expressMaxMinutes: 60,
    },
    today: "2026-10-18",
    version: 1,
    ...extra,
  };
}

function apiError(status: number, message: string): Error {
  return Object.assign(new Error(message), { status, code: "x" });
}

describe("admin scope", () => {
  const all = [dealer("a"), dealer("b")];

  it("group admin sees every dealership and is group-wide", () => {
    const user = { role: Role.Admin, dealershipId: undefined };
    expect(isGroupAdmin(user)).toBe(true);
    expect(adminDealerships(user, all).map((d) => d.id)).toEqual(["a", "b"]);
  });

  it("dealership admin sees only their own dealership and is not group-wide", () => {
    const user = { role: Role.Admin, dealershipId: "b" };
    expect(isGroupAdmin(user)).toBe(false);
    expect(adminDealerships(user, all).map((d) => d.id)).toEqual(["b"]);
  });

  it("only group admins get the catalog and outbox tabs", () => {
    expect(adminTabs(true)).toContain("catalog");
    expect(adminTabs(true)).toContain("outbox");
    expect(adminTabs(false)).not.toContain("catalog");
    expect(adminTabs(false)).not.toContain("outbox");
  });

  it("non-admins configure nothing", () => {
    expect(isGroupAdmin({ role: Role.Dispatcher })).toBe(false);
    expect(adminDealerships({ role: Role.Dispatcher }, all)).toEqual([]);
    expect(adminDealerships(null, all)).toEqual([]);
  });
});

describe("adminErrorText", () => {
  it("keeps the server's stranded-booking explanation on 409", () => {
    expect(adminErrorText(apiError(409, "2 upcoming appointment(s) fall outside the new hours"))).toBe(
      "Not saved: 2 upcoming appointment(s) fall outside the new hours",
    );
  });
  it("explains 403 and 412 in plain words", () => {
    expect(adminErrorText(apiError(403, "forbidden"))).toMatch(/permission/);
    expect(adminErrorText(apiError(412, "stale"))).toMatch(/latest values/);
  });
  it("passes other errors through", () => {
    expect(adminErrorText(new Error("network down"))).toBe("network down");
  });
});

describe("hoursProblem", () => {
  it.each([
    ["08:00", "17:00", "", "", ""],
    ["08:00", "17:00", "12:00", "13:00", ""],
    ["", "17:00", "", "", "Opening and closing times are required."],
    ["17:00", "08:00", "", "", "Closing time must be after opening time."],
    ["08:00", "17:00", "12:00", "", "Set both lunch start and lunch end, or untick lunch."],
    ["08:00", "17:00", "13:00", "12:00", "Lunch must end after it starts."],
    ["09:00", "17:00", "08:00", "09:30", "Lunch must sit inside the opening hours."],
  ])("%s–%s lunch %s–%s", (open, close, ls, le, want) => {
    expect(hoursProblem(open, close, ls, le)).toBe(want);
  });
});

describe("weekly hours", () => {
  const cal: DealershipCalendar = {
    dealershipId: "a",
    version: 3,
    weekly: [
      { weekday: 6, closed: false, openTime: "09:00", closeTime: "13:00" },
      { weekday: 7, closed: true },
    ],
    closures: [],
  };

  it("lays overrides over the defaults and round-trips to the PUT body", () => {
    const rows = weekRows(cal, dealer("a"));
    expect(rows.map((r) => r.mode)).toEqual(["default", "default", "default", "default", "default", "custom", "closed"]);
    expect(rows[0]).toMatchObject({ openTime: "08:00", closeTime: "17:00", lunch: true });
    expect(rows[5]).toMatchObject({ openTime: "09:00", closeTime: "13:00", lunch: false });
    expect(toWeekly(rows)).toEqual(cal.weekly);
  });

  it("sends lunch only for custom days that keep it, and nothing for default days", () => {
    const rows = weekRows(null, dealer("a"));
    rows[0] = { ...rows[0], mode: "custom" };
    expect(toWeekly(rows)).toEqual([
      { weekday: 1, closed: false, openTime: "08:00", closeTime: "17:00", lunchStart: "12:00", lunchEnd: "13:00" },
    ]);
  });

  it("names the first invalid custom weekday", () => {
    const rows = weekRows(null, dealer("a"));
    rows[2] = { ...rows[2], mode: "custom", openTime: "18:00" };
    expect(weekProblem(rows)).toBe("Wednesday: Closing time must be after opening time.");
    rows[2] = { ...rows[2], mode: "closed" };
    expect(weekProblem(rows)).toBe("");
  });
});

describe("hoursPatch", () => {
  const d = dealer("a");

  it("is empty when nothing changed", () => {
    expect(hoursPatch(d, dealerHoursForm(d))).toEqual({});
  });

  it("sends only the changed times", () => {
    expect(hoursPatch(d, { ...dealerHoursForm(d), closeTime: "18:00" })).toEqual({ closeTime: "18:00" });
  });

  it("clears lunch with two empty strings", () => {
    expect(hoursPatch(d, { ...dealerHoursForm(d), lunch: false })).toEqual({ lunchStart: "", lunchEnd: "" });
  });

  it("adds lunch to a dealership without one", () => {
    const noLunch = dealer("b", { lunchStart: undefined, lunchEnd: undefined });
    const f = dealerHoursForm(noLunch);
    expect(f.lunch).toBe(false);
    expect(hoursPatch(noLunch, { ...f, lunch: true })).toEqual({ lunchStart: "12:00", lunchEnd: "13:00" });
  });
});

describe("vinProblem", () => {
  it("accepts a valid VIN in any case", () => {
    expect(vinProblem("1hgcm82633a004352")).toBe("");
  });
  it("rejects wrong length and I/O/Q", () => {
    expect(vinProblem("123")).toMatch(/17/);
    expect(vinProblem("1HGCM82633A00435O")).toMatch(/I, O or Q/);
  });
});

describe("customerProblem", () => {
  it("accepts a name with a phone or an email", () => {
    expect(customerProblem({ name: "Ann", phone: "(02) 9876-5432", email: "" })).toBe("");
    expect(customerProblem({ name: "Ann", phone: "", email: "ann@example.com" })).toBe("");
  });
  it("needs a name and a way to reach them", () => {
    expect(customerProblem({ name: " ", phone: "0400111222", email: "" })).toMatch(/name/);
    expect(customerProblem({ name: "Ann", phone: "", email: "" })).toMatch(/phone number or an email/);
  });
  it("rejects malformed contact details", () => {
    expect(customerProblem({ name: "Ann", phone: "", email: "ann@localhost" })).toMatch(/email/);
    expect(customerProblem({ name: "Ann", phone: "0400 CALL", email: "" })).toMatch(/digits/);
    expect(customerProblem({ name: "Ann", phone: "+1 (2) 3", email: "" })).toMatch(/digits/);
  });
});

describe("skill sets", () => {
  it("finds removed skills and compares sets ignoring order", () => {
    expect(removedSkills([1, 2, 3], [3, 2])).toEqual([1]);
    expect(removedSkills([2], [1, 2])).toEqual([]);
    expect(sameSkills([1, 3], [3, 1])).toBe(true);
    expect(sameSkills([1, 3], [1])).toBe(false);
  });
});
