// Small UI behaviours from the review: extend choice, live regions, role-aware wording, the
// first-available list's pressed state, and "Today" following the clock.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ErrorBanner } from "./ErrorBanner";
import { ExtendControl, extendProblem } from "./ExtendControl";
import { slotReasonText } from "./labels";
import { NextSlotList } from "./SlotGrid";
import { Toast } from "./Toast";
import { WorkshopBar } from "./WorkshopBar";
import { dealership } from "./testFixtures";
import type { NextAvailability } from "./types";

afterEach(() => vi.useRealTimers());

describe("ExtendControl", () => {
  it("extends by a preset or a custom 15–240 minutes in 15-minute steps", async () => {
    const onExtend = vi.fn();
    render(<ExtendControl busy={false} onExtend={onExtend} />);
    await userEvent.click(screen.getByRole("button", { name: "Extend +15m" }));
    expect(onExtend).toHaveBeenLastCalledWith(15);

    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Extend by" }), "60");
    await userEvent.click(screen.getByRole("button", { name: "Extend +1h" }));
    expect(onExtend).toHaveBeenLastCalledWith(60);

    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Extend by" }), "custom");
    const minutes = screen.getByRole("spinbutton", { name: "Extend minutes" });
    await userEvent.clear(minutes);
    await userEvent.type(minutes, "50");
    expect(screen.getByRole("button", { name: "Extend" })).toHaveProperty("disabled", true);
    await userEvent.clear(minutes);
    await userEvent.type(minutes, "45");
    await userEvent.click(screen.getByRole("button", { name: "Extend +45m" }));
    expect(onExtend).toHaveBeenLastCalledWith(45);
  });

  it("states the API's bounds", () => {
    expect(extendProblem(15)).toBe("");
    expect(extendProblem(240)).toBe("");
    expect(extendProblem(255)).toMatch(/15–240 minutes/);
    expect(extendProblem(10)).toMatch(/steps of 15/);
  });
});

describe("live regions", () => {
  it("keeps the toast's and the error's regions mounted while empty, so later messages are announced", () => {
    const { rerender } = render(
      <>
        <Toast message="" onDismiss={vi.fn()} />
        <ErrorBanner message="" />
      </>,
    );
    const status = screen.getByRole("status");
    expect(status.textContent).toBe("");
    expect(document.querySelector('[aria-live="assertive"]')).not.toBeNull();
    rerender(
      <>
        <Toast message="Booked." onDismiss={vi.fn()} />
        <ErrorBanner message="Not saved" />
      </>,
    );
    expect(screen.getByRole("status")).toBe(status);
    expect(status.textContent).toContain("Booked.");
    expect(screen.getByRole("alert").textContent).toContain("Not saved");
  });
});

describe("wording and state", () => {
  it("words the car's own clash for the desk, not as 'your car'", () => {
    expect(slotReasonText("vehicle_busy")).toBe("Your car is booked then");
    expect(slotReasonText("vehicle_busy", true)).toBe("This car is already booked then");
    expect(slotReasonText("no_bay", true)).toBe("No suitable bay free");
  });

  it("marks the picked first-available start as pressed", () => {
    const next: NextAvailability = {
      timezone: "Europe/London",
      durationMinutes: 60,
      searchedFrom: "2026-10-19",
      searchedTo: "2026-11-02",
      slots: [
        { date: "2026-10-21", start: "2026-10-21T08:00:00Z", end: "2026-10-21T09:00:00Z", freeBays: 1, freeTechnicians: 1 },
        { date: "2026-10-21", start: "2026-10-21T09:00:00Z", end: "2026-10-21T10:00:00Z", freeBays: 1, freeTechnicians: 1 },
      ],
    };
    const picked = { start: "2026-10-21T09:00:00Z", end: "2026-10-21T10:00:00Z", available: true, freeBays: 1, freeTechnicians: 1 };
    render(<NextSlotList next={next} selectedSlot={picked} busy={false} onPick={vi.fn()} />);
    expect(screen.getByRole("button", { name: /09:00/ }).getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("button", { name: /10:00/ }).getAttribute("aria-pressed")).toBe("true");
  });

  it("jumps 'Today' to the dealership's real today after midnight, not the day the catalog was loaded", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    // 00:30 on Tue 20 Oct in London; the catalog still says the 19th.
    vi.setSystemTime(new Date("2026-10-19T23:30:00Z"));
    const onDateChange = vi.fn();
    render(
      <WorkshopBar dealerships={[dealership]} dealershipId="d1" onDealershipChange={vi.fn()} date="2026-10-19" onDateChange={onDateChange} />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Today" }));
    expect(onDateChange).toHaveBeenCalledWith("2026-10-20");
  });
});
