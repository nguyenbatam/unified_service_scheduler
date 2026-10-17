import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { NextSlotList, SlotGrid } from "./SlotGrid";
import { makeAvailability } from "./testFixtures";
import type { Availability, NextAvailability } from "./types";

/** 09:00 recommended, 10:00 free but not, 11:00 taken (a stray recommended flag must not show). */
function bestFitGrid(): Availability {
  const grid = makeAvailability("2026-10-19", ["2026-10-19T08:00:00Z", "2026-10-19T09:00:00Z", "2026-10-19T10:00:00Z"]);
  grid.slots[0] = { ...grid.slots[0], recommended: true };
  grid.slots[2] = { ...grid.slots[2], available: false, freeBays: 0, reason: "no_bay", recommended: true };
  return grid;
}

const next: NextAvailability = {
  timezone: "Europe/London",
  durationMinutes: 60,
  searchedFrom: "2026-10-19",
  searchedTo: "2026-11-02",
  slots: [
    { date: "2026-10-21", start: "2026-10-21T08:00:00Z", end: "2026-10-21T09:00:00Z", freeBays: 1, freeTechnicians: 1, recommended: true },
    { date: "2026-10-21", start: "2026-10-21T09:00:00Z", end: "2026-10-21T10:00:00Z", freeBays: 2, freeTechnicians: 2 },
  ],
};

describe("SlotGrid best fit", () => {
  it("marks recommended free starts, keeps the others bookable, and tells customers why", () => {
    render(<SlotGrid availability={bestFitGrid()} selectedSlot={null} busy={false} onSelect={vi.fn()} onConfirm={vi.fn()} />);
    const best = screen.getByRole("button", { name: "09:00, available, best fit" });
    expect(within(best).getByText("Best fit")).toBeTruthy();
    expect(best.className).toContain("recommended");

    const plain = screen.getByRole("button", { name: "10:00, available" });
    expect(plain).toHaveProperty("disabled", false);
    expect(within(plain).queryByText("Best fit")).toBeNull();

    const taken = screen.getByRole("button", { name: /^11:00, / });
    expect(taken.getAttribute("aria-label")).not.toContain("best fit");
    expect(within(taken).queryByText("Best fit")).toBeNull();

    expect(screen.getByText(/recommended times help the workshop fit more jobs/)).toBeTruthy();
  });

  it("explains best fit in workshop terms to staff", () => {
    render(
      <SlotGrid availability={bestFitGrid()} selectedSlot={null} busy={false} showCapacity onSelect={vi.fn()} onConfirm={vi.fn()} />,
    );
    expect(screen.getByRole("button", { name: "09:00, available, best fit" })).toBeTruthy();
    expect(screen.getByText(/keeping long free stretches for long jobs/)).toBeTruthy();
    expect(screen.queryByText(/help the workshop fit more jobs/)).toBeNull();
  });

  it("groups the starts by day under a heading that names what the list is", () => {
    const twoDays: NextAvailability = {
      ...next,
      slots: [...next.slots, { date: "2026-10-22", start: "2026-10-22T08:00:00Z", end: "2026-10-22T09:00:00Z", freeBays: 1, freeTechnicians: 1 }],
    };
    const { unmount } = render(<NextSlotList next={twoDays} selectedSlot={null} busy={false} onPick={vi.fn()} />);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toMatch(/^Suggested times · 60 min/);
    const wed = screen.getByRole("group", { name: "Wed, 21 Oct 2026" });
    expect(within(wed).getAllByRole("button").map((b) => b.textContent)).toEqual([
      expect.stringContaining("09:00"),
      expect.stringContaining("10:00"),
    ]);
    expect(within(screen.getByRole("group", { name: "Thu, 22 Oct 2026" })).getAllByRole("button")).toHaveLength(1);
    unmount();
    render(<NextSlotList next={twoDays} selectedSlot={null} busy={false} showCapacity onPick={vi.fn()} />);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toMatch(/^Next available/);
  });

  it("shows no legend when no start is recommended", () => {
    const grid = makeAvailability("2026-10-19", ["2026-10-19T08:00:00Z"]);
    render(<SlotGrid availability={grid} selectedSlot={null} busy={false} onSelect={vi.fn()} onConfirm={vi.fn()} />);
    expect(screen.queryByText(/Best fit/)).toBeNull();
  });
});

describe("NextSlotList best fit", () => {
  it("marks recommended starts among the suggestions and explains them", () => {
    const onPick = vi.fn();
    render(<NextSlotList next={next} selectedSlot={null} busy={false} onPick={onPick} />);
    const best = screen.getByRole("button", { name: "Wed, 21 Oct 2026 09:00, free, best fit" });
    expect(best.className).toContain("recommended");
    const plain = screen.getByRole("button", { name: "Wed, 21 Oct 2026 10:00, free" });
    expect(within(plain).queryByText("Best fit")).toBeNull();
    expect(plain).toHaveProperty("disabled", false);
    expect(screen.getByText(/recommended times help the workshop fit more jobs/)).toBeTruthy();
  });

  it("groups the starts by day under a heading that names what the list is", () => {
    const twoDays: NextAvailability = {
      ...next,
      slots: [...next.slots, { date: "2026-10-22", start: "2026-10-22T08:00:00Z", end: "2026-10-22T09:00:00Z", freeBays: 1, freeTechnicians: 1 }],
    };
    const { unmount } = render(<NextSlotList next={twoDays} selectedSlot={null} busy={false} onPick={vi.fn()} />);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toMatch(/^Suggested times · 60 min/);
    const wed = screen.getByRole("group", { name: "Wed, 21 Oct 2026" });
    expect(within(wed).getAllByRole("button").map((b) => b.textContent)).toEqual([
      expect.stringContaining("09:00"),
      expect.stringContaining("10:00"),
    ]);
    expect(within(screen.getByRole("group", { name: "Thu, 22 Oct 2026" })).getAllByRole("button")).toHaveLength(1);
    unmount();
    render(<NextSlotList next={twoDays} selectedSlot={null} busy={false} showCapacity onPick={vi.fn()} />);
    expect(screen.getByRole("heading", { level: 3 }).textContent).toMatch(/^Next available/);
  });

  it("shows no legend when no start is recommended", () => {
    render(
      <NextSlotList
        next={{ ...next, slots: next.slots.map((s) => ({ ...s, recommended: false })) }}
        selectedSlot={null}
        busy={false}
        onPick={vi.fn()}
      />,
    );
    expect(screen.queryByText(/Best fit/)).toBeNull();
  });
});
