import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { AppointmentPanel } from "./AppointmentPanel";
import { ConfirmHost, confirmAction } from "./ConfirmDialog";
import { Tabs } from "./Tabs";
import { DayBoard } from "./DayBoard";
import { WorkshopBar } from "./WorkshopBar";
import { FETCH_ALL_MAX } from "./api";
import { SlotGrid } from "./SlotGrid";
import { dealership, makeAppointment, makeAvailability, makeSchedule } from "./testFixtures";
import { Status, type Appointment } from "./types";

afterEach(() => vi.restoreAllMocks());

const grid = makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z", "2026-10-19T10:00:00Z"]);

describe("SlotGrid", () => {
  it("books in normal mode and names the picked day and time", async () => {
    const onConfirm = vi.fn();
    render(
      <SlotGrid availability={grid} selectedSlot={grid.slots[1]} busy={false} onSelect={vi.fn()} onConfirm={onConfirm} />,
    );
    expect(screen.getByRole("heading", { name: /Mon, 19 Oct 2026/ })).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Confirm booking · Mon, 19 Oct 2026 11:00" }));
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", { name: /Reschedule/ })).toBeNull();
  });

  it("reschedules as the main action and keeps booking a new one secondary", async () => {
    const onReschedule = vi.fn();
    const onConfirm = vi.fn();
    const onStop = vi.fn();
    render(
      <SlotGrid
        availability={grid}
        selectedSlot={grid.slots[1]}
        busy={false}
        onSelect={vi.fn()}
        onConfirm={onConfirm}
        rescheduleTarget={makeAppointment()}
        onReschedule={onReschedule}
        onStopReschedule={onStop}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Reschedule to Mon, 19 Oct 2026 11:00" }));
    expect(onReschedule).toHaveBeenCalledWith(grid.slots[1]);
    expect(onConfirm).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Book as an extra appointment instead" }));
    expect(onConfirm).toHaveBeenCalledOnce();
    await userEvent.click(screen.getByRole("button", { name: "Stop rescheduling" }));
    expect(onStop).toHaveBeenCalledOnce();
  });

  it("refuses to reschedule onto the appointment's current time", () => {
    render(
      <SlotGrid
        availability={grid}
        selectedSlot={grid.slots[0]}
        busy={false}
        onSelect={vi.fn()}
        onConfirm={vi.fn()}
        rescheduleTarget={makeAppointment()}
        onReschedule={vi.fn()}
      />,
    );
    expect((screen.getByRole("button", { name: "Pick a new time to reschedule" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("explains unavailable slots in words", () => {
    const busyGrid = { ...grid, slots: [{ ...grid.slots[0], available: false, freeBays: 0, reason: "staging_full" }] };
    render(<SlotGrid availability={busyGrid} selectedSlot={null} busy={false} onSelect={vi.fn()} onConfirm={vi.fn()} />);
    expect(screen.getByText("Check-in area full")).toBeTruthy();
  });
});

function panel(appointment: Appointment, isStaff: boolean, onAction = vi.fn()) {
  render(
    <AppointmentPanel
      appointment={appointment}
      timezone="Europe/London"
      busy={false}
      isStaff={isStaff}
      policy={dealership.policy}
      schedule={null}
      rescheduling={false}
      onAction={onAction}
      onStartReschedule={vi.fn()}
      onClose={vi.fn()}
    />,
  );
  return onAction;
}

const future = () => makeAppointment({ startAt: new Date(Date.now() + 3 * 86_400_000).toISOString(), endAt: new Date(Date.now() + 3 * 86_400_000 + 3_600_000).toISOString() });

describe("AppointmentPanel", () => {
  it("offers workshop actions to staff only", () => {
    panel(future(), false);
    expect(screen.queryByRole("button", { name: "Check in" })).toBeNull();
    expect(screen.queryByRole("button", { name: "No-show" })).toBeNull();
    expect(screen.getByRole("button", { name: "Cancel appointment" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reschedule…" })).toBeTruthy();
  });

  it("shows the actions of each status", () => {
    panel(makeAppointment({ status: Status.InProgress }), true);
    expect(screen.getByRole("button", { name: "Complete (release bay/tech)" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Cancel appointment" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reschedule…" })).toBeNull();
    expect(screen.getByText("In progress")).toBeTruthy();
  });

  it("asks before cancelling and sends nothing when the user backs out", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const onAction = panel(future(), true);
    await userEvent.click(screen.getByRole("button", { name: "Cancel appointment" }));
    expect(confirm).toHaveBeenCalled();
    expect(onAction).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    await userEvent.click(screen.getByRole("button", { name: "Cancel appointment" }));
    expect(onAction).toHaveBeenCalledOnce();
  });

  it("locks customer changes after the cutoff and says why", () => {
    const soon = makeAppointment({
      startAt: new Date(Date.now() + 30 * 60_000).toISOString(),
      endAt: new Date(Date.now() + 90 * 60_000).toISOString(),
    });
    panel(soon, false);
    expect((screen.getByRole("button", { name: "Cancel appointment" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Reschedule…" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/Online changes close 1h before the start/)).toBeTruthy();
  });

  it("does not lock staff after the cutoff", () => {
    const soon = makeAppointment({
      startAt: new Date(Date.now() + 30 * 60_000).toISOString(),
      endAt: new Date(Date.now() + 90 * 60_000).toISOString(),
    });
    panel(soon, true);
    expect((screen.getByRole("button", { name: "Cancel appointment" }) as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("DayBoard", () => {
  it("draws finished work faded and leaves cancelled work out", () => {
    render(
      <DayBoard
        schedule={makeSchedule([
          makeAppointment({ id: "done", status: Status.Completed }),
          makeAppointment({ id: "gone", status: Status.Cancelled, startAt: "2026-10-19T12:00:00Z", endAt: "2026-10-19T13:00:00Z" }),
          makeAppointment({ id: "next", startAt: "2026-10-19T09:30:00Z", endAt: "2026-10-19T10:30:00Z" }),
        ])}
        onSelect={vi.fn()}
      />,
    );
    const blocks = screen.getAllByRole("button");
    expect(blocks).toHaveLength(2);
    expect(blocks.find((b) => b.textContent?.includes("Completed"))?.className).toContain("finished");
    expect(blocks.find((b) => b.textContent?.includes("Confirmed"))?.className).not.toContain("finished");
  });

  it("draws the day's lunch band from the schedule, and none without lunch", () => {
    const { container, rerender } = render(
      <DayBoard schedule={{ ...makeSchedule([]), lunchStart: "12:00", lunchEnd: "13:00" }} onSelect={vi.fn()} />,
    );
    const band = container.querySelector<HTMLElement>(".lunch-band");
    expect(band).not.toBeNull();
    expect(band?.title).toBe("Lunch 12:00–13:00");
    // 08:00–17:00 board: lunch starts 4/9 of the way down and lasts 1/9.
    expect(parseFloat(band?.style.top ?? "")).toBeCloseTo((4 / 9) * 100, 1);
    expect(parseFloat(band?.style.height ?? "")).toBeCloseTo((1 / 9) * 100, 1);
    rerender(<DayBoard schedule={makeSchedule([])} onSelect={vi.fn()} />);
    expect(container.querySelector(".lunch-band")).toBeNull();
  });
});

describe("WorkshopBar day controls", () => {
  it("moves to the previous and next day and back to today", async () => {
    const onDateChange = vi.fn();
    render(
      <WorkshopBar
        dealerships={[dealership]}
        dealershipId="d1"
        onDealershipChange={vi.fn()}
        date="2026-10-31"
        onDateChange={onDateChange}
      />,
    );
    expect(screen.getByText(/times in London time/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Next day" }));
    await userEvent.click(screen.getByRole("button", { name: "Previous day" }));
    await userEvent.click(screen.getByRole("button", { name: "Today" }));
    expect(onDateChange.mock.calls).toEqual([["2026-11-01"], ["2026-10-30"], ["2026-10-19"]]);
  });

  it("says when the dealership list stopped at fetchAll's cap", () => {
    const props = { dealerships: [dealership], dealershipId: "d1", onDealershipChange: vi.fn(), date: "", onDateChange: vi.fn() };
    const { rerender } = render(<WorkshopBar {...props} />);
    expect(screen.queryByRole("note")).toBeNull();
    rerender(<WorkshopBar {...props} truncated />);
    expect(screen.getByRole("note").textContent).toBe(`Only the first ${FETCH_ALL_MAX} dealerships are listed.`);
  });
});

describe("DayBoard load states", () => {
  it("shows a failed load with a retry instead of loading forever", async () => {
    const onRetry = vi.fn();
    render(<DayBoard schedule={null} error="forbidden" onRetry={onRetry} onSelect={vi.fn()} />);
    expect(screen.queryByText("Loading the day board…")).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain("Could not load the day board: forbidden");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledOnce();
  });
});

describe("ConfirmHost", () => {
  function Asker({ onAnswer }: { onAnswer: (ok: boolean) => void }) {
    return (
      <button type="button" onClick={() => void confirmAction("Cancel this appointment?").then(onAnswer)}>
        Cancel appointment
      </button>
    );
  }

  it("starts on the safe answer, keeps focus inside, and returns it to the opener", async () => {
    const onAnswer = vi.fn();
    render(
      <>
        <ConfirmHost />
        <Asker onAnswer={onAnswer} />
      </>,
    );
    const opener = screen.getByRole("button", { name: "Cancel appointment" });
    await userEvent.click(opener);
    const dialog = screen.getByRole("alertdialog", { name: "Cancel this appointment?" });
    const back = screen.getByRole("button", { name: "Go back" });
    expect(dialog).toBeTruthy();
    expect(document.activeElement).toBe(back);

    await userEvent.keyboard("{Enter}"); // Enter on the focused safe button backs out
    expect(onAnswer).toHaveBeenLastCalledWith(false);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(document.activeElement).toBe(opener);

    await userEvent.click(opener);
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Yes, continue" }));
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Go back" }));
    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Yes, continue" }));
    await userEvent.keyboard("{Escape}");
    expect(onAnswer).toHaveBeenLastCalledWith(false);
    expect(document.activeElement).toBe(opener);

    await userEvent.click(opener);
    await userEvent.click(screen.getByRole("button", { name: "Yes, continue" }));
    expect(onAnswer).toHaveBeenLastCalledWith(true);
  });
});

describe("Tabs", () => {
  function Harness() {
    const [tab, setTab] = useState<"a" | "b" | "c">("a");
    return (
      <Tabs
        label="Sections"
        idPrefix="t"
        tabs={[
          { id: "a", label: "Alpha" },
          { id: "b", label: "Beta" },
          { id: "c", label: "Gamma" },
        ]}
        active={tab}
        onChange={setTab}
      >
        <p>Panel {tab}</p>
      </Tabs>
    );
  }

  it("follows the ARIA tabs pattern: one tab stop, arrow keys, labelled panel", async () => {
    render(<Harness />);
    const alpha = screen.getByRole("tab", { name: "Alpha" });
    expect(alpha.getAttribute("tabindex")).toBe("0");
    expect(screen.getByRole("tab", { name: "Beta" }).getAttribute("tabindex")).toBe("-1");
    expect(screen.getByRole("tabpanel", { name: "Alpha" }).textContent).toBe("Panel a");
    expect(alpha.getAttribute("aria-controls")).toBe(screen.getByRole("tabpanel").id);

    alpha.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "Beta" }));
    expect(screen.getByRole("tabpanel", { name: "Beta" }).textContent).toBe("Panel b");
    await userEvent.keyboard("{End}");
    expect(screen.getByRole("tab", { name: "Gamma" }).getAttribute("aria-selected")).toBe("true");
    await userEvent.keyboard("{ArrowRight}"); // wraps
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "Alpha" }));
    await userEvent.keyboard("{ArrowLeft}");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "Gamma" }));
  });
});
