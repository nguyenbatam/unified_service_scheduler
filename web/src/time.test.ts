import { describe, expect, it } from "vitest";
import {
  shiftDay,
  boardHours,
  boardLayout,
  boardOffset,
  clockOf,
  formatDayLabel,
  minutesOnDay,
  formatClock,
  formatClockRange,
  formatDate,
  formatMinutes,
  parseClockMinutes,
  zonedToIso,
  zoneAbbr,
  zoneLabel,
} from "./time";

describe("time helpers", () => {
  it("formats the civil date in the dealership zone", () => {
    expect(formatDate("2026-06-16T23:30:00Z", "Europe/London")).toBe("2026-06-17");
  });

  it("formats London wall clock", () => {
    expect(formatClock("2026-06-16T08:00:00Z", "Europe/London")).toBe("09:00");
  });

  it("formats a clock range", () => {
    expect(
      formatClockRange("2026-06-16T08:00:00Z", "2026-06-16T09:30:00Z", "Europe/London"),
    ).toBe("09:00–10:30");
  });

  it("parses HH:MM to minutes", () => {
    expect(parseClockMinutes("08:00")).toBe(480);
    expect(parseClockMinutes("17:00")).toBe(1020);
  });

  it("places a block on the day board", () => {
    const layout = boardLayout(
      "2026-06-16T08:00:00Z",
      "2026-06-16T09:30:00Z",
      "2026-06-16",
      "08:00",
      "17:00",
      "Europe/London",
    );
    // 09:00–10:30 BST on an 08:00–17:00 board
    expect(layout.topPct).toBeCloseTo((60 / 540) * 100, 5);
    expect(layout.heightPct).toBeCloseTo((90 / 540) * 100, 5);
    expect(layout.minutes).toBe(90);
    expect(layout.clippedStart || layout.clippedEnd).toBe(false);
  });

  it("clips a block held past closing to the bottom of the board", () => {
    // A flagged overrun from 15:00 BST, drawn past closing.
    const layout = boardLayout(
      "2026-06-16T14:00:00Z",
      "2026-06-16T23:00:00Z",
      "2026-06-16",
      "08:00",
      "17:00",
      "Europe/London",
    );
    expect(layout.topPct).toBeCloseTo((420 / 540) * 100, 5);
    expect(layout.topPct + layout.heightPct).toBeCloseTo(100, 5);
    expect(layout.clippedEnd).toBe(true);
  });

  it("starts a block from the previous day at the top", () => {
    const layout = boardLayout(
      "2026-06-15T20:00:00Z",
      "2026-06-16T08:00:00Z",
      "2026-06-16",
      "08:00",
      "17:00",
      "Europe/London",
    );
    expect(layout.topPct).toBe(0);
    expect(layout.heightPct).toBeCloseTo((60 / 540) * 100, 5);
    expect(layout.clippedStart).toBe(true);
  });

  it("counts minutes across days from the board's midnight", () => {
    expect(minutesOnDay("2026-06-16T23:00:00Z", "2026-06-16", "Europe/London")).toBe(1440);
    expect(minutesOnDay("2026-06-15T22:00:00Z", "2026-06-16", "Europe/London")).toBe(-60);
  });

  it("places clock times and hours on the board axis", () => {
    expect(boardOffset("12:30", "08:00", "17:00")).toBeCloseTo((270 / 540) * 100, 5);
    expect(boardOffset("18:00", "08:00", "17:00")).toBeNull();
    expect(boardHours("08:30", "11:00").map(clockOf)).toEqual(["09:00", "10:00", "11:00"]);
    expect(formatDayLabel("2026-10-19")).toBe("Mon, 19 Oct 2026");
  });

  it("converts dealership wall time to UTC", () => {
    expect(zonedToIso("2026-06-16T09:00", "Europe/London")).toBe("2026-06-16T08:00:00.000Z");
    expect(zonedToIso("2026-12-01T09:00", "Europe/London")).toBe("2026-12-01T09:00:00.000Z");
    expect(zonedToIso("2026-06-16T09:00", "Asia/Ho_Chi_Minh")).toBe("2026-06-16T02:00:00.000Z");
    expect(zonedToIso("2026-06-16T09:00", "UTC")).toBe("2026-06-16T09:00:00.000Z");
  });

  it("formats policy durations", () => {
    expect([45, 60, 90, 1440, 2880].map(formatMinutes)).toEqual(["45m", "1h", "1h30m", "1d", "2d"]);
  });
});

describe("shiftDay", () => {
  it("does calendar arithmetic across month, year, and DST boundaries", () => {
    expect(shiftDay("2026-10-31", 1)).toBe("2026-11-01");
    expect(shiftDay("2026-10-25", 1)).toBe("2026-10-26"); // London clocks go back that night
    expect(shiftDay("2027-01-01", -1)).toBe("2026-12-31");
    expect(shiftDay("2028-02-28", 1)).toBe("2028-02-29");
    expect(shiftDay("not a day", 1)).toBe("not a day");
  });
});

describe("zone labels", () => {
  it("names the dealership's zone and its abbreviation at that instant", () => {
    expect(zoneAbbr("Europe/London", "2026-06-16T12:00:00Z")).toBe("BST");
    expect(zoneAbbr("Europe/London", "2026-12-16T12:00:00Z")).toBe("GMT");
    expect(zoneLabel("Europe/London", "2026-06-16T12:00:00Z")).toBe("London time (BST)");
    expect(zoneLabel("America/New_York", "2026-06-16T12:00:00Z")).toMatch(/^New York time \(/);
    expect(zoneLabel("UTC", "2026-06-16T12:00:00Z")).toBe("UTC");
  });
});
