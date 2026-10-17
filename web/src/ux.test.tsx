// Component behaviour from the UI review: plain words for customers, time-gated workshop actions,
// readable board blocks, demo sign-in, inline vehicle errors, and kept-mounted admin tabs.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useEffect } from "react";
import { AppointmentPanel } from "./AppointmentPanel";
import { AuthPanel } from "./AuthPanel";
import { VehicleForm } from "./CustomerPanels";
import { DayBoard } from "./DayBoard";
import { StaffLayout } from "./Layouts";
import { ServicePicker } from "./ServicePicker";
import { SlotGrid } from "./SlotGrid";
import { scopeQuery } from "./StaffAppointments";
import { dealership, makeAppointment, makeAvailability, makeSchedule, on, service, stubFetch } from "./testFixtures";
import { Role, Status, type Appointment, type Schedule } from "./types";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  localStorage.clear();
});

function panel(appointment: Appointment, isStaff: boolean, schedule: Schedule | null = makeSchedule([])) {
  render(
    <AppointmentPanel
      appointment={appointment}
      timezone="Europe/London"
      busy={false}
      isStaff={isStaff}
      policy={dealership.policy}
      schedule={schedule}
      rescheduling={false}
      onAction={vi.fn()}
      onStartReschedule={vi.fn()}
      onClose={vi.fn()}
    />,
  );
}

describe("AppointmentPanel timing", () => {
  it("disables check-in before the appointment day and a no-show before the start, saying why", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-18T12:00:00Z"));
    panel(makeAppointment(), true);
    const checkIn = screen.getByRole("button", { name: "Check in" }) as HTMLButtonElement;
    const noShow = screen.getByRole("button", { name: "No-show" }) as HTMLButtonElement;
    expect(checkIn.disabled).toBe(true);
    expect(checkIn.title).toBe("Check-in opens on the appointment day (Mon, 19 Oct 2026).");
    expect(noShow.disabled).toBe(true);
    expect(screen.getByText(/A no-show can be marked once the booked start \(10:00\) has passed/)).toBeTruthy();
    expect(screen.getByText("BST")).toBeTruthy();
  });

  it("enables both on the day once the start has passed", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-19T09:10:00Z"));
    panel(makeAppointment(), true);
    expect((screen.getByRole("button", { name: "Check in" }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole("button", { name: "No-show" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("enables a no-show right at the booked start, without waiting for the minute tick", () => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
    vi.setSystemTime(new Date("2026-10-19T08:59:30Z")); // the start is 09:00Z (10:00 BST)
    panel(makeAppointment(), true);
    const noShow = () => screen.getByRole("button", { name: "No-show" }) as HTMLButtonElement;
    expect(noShow().disabled).toBe(true);
    act(() => void vi.advanceTimersByTime(30_001));
    expect(noShow().disabled).toBe(false);
  });

  it("re-checks a reinstate against the clock on click: just past the end it is not sent", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-19T09:30:00Z"));
    const onAction = vi.fn();
    render(
      <AppointmentPanel
        appointment={makeAppointment({ status: Status.NoShow })}
        timezone="Europe/London"
        busy={false}
        isStaff
        policy={dealership.policy}
        schedule={makeSchedule([])}
        rescheduling={false}
        onAction={onAction}
        onStartReschedule={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const reinstate = screen.getByRole("button", { name: "Reinstate (arrived late)" }) as HTMLButtonElement;
    expect(reinstate.disabled).toBe(false);
    vi.setSystemTime(new Date("2026-10-19T10:00:30Z")); // the booked window (to 10:00Z) is over; no re-render yet
    fireEvent.click(reinstate);
    expect(onAction).not.toHaveBeenCalled();
    expect(reinstate.disabled).toBe(true);
    expect(screen.getByText("The booked window has ended — book a new appointment instead.")).toBeTruthy();
  });

  it("shows customers no row version or bay type", () => {
    panel(makeAppointment({ version: 3, startAt: "2030-01-07T10:00:00Z", endAt: "2030-01-07T11:00:00Z" }), false);
    expect(screen.queryByText(/v3/)).toBeNull();
    expect(screen.getByText("Express 1")).toBeTruthy();
    expect(screen.getByText("Waiting on site")).toBeTruthy();
  });

  it("reads another dealership's bays and technicians to reassign instead of hiding it", async () => {
    const schedule = vi.fn(() => ({
      body: { ...makeSchedule([]), technicians: [{ id: "t1", name: "Taylor" }, { id: "t9", name: "Robin" }] },
    }));
    stubFetch(on("GET", "/api/v1/schedule", schedule));
    panel(makeAppointment({ dealershipId: "d2", startAt: "2030-01-07T10:00:00Z", endAt: "2030-01-07T11:00:00Z" }), true, null);
    const tech = (await screen.findByRole("combobox", { name: "Technician" })) as HTMLSelectElement;
    expect(Array.from(tech.options).map((o) => o.textContent)).toContain("Robin");
    expect(schedule).toHaveBeenCalledOnce();
  });

  it("explains when the other dealership's schedule cannot be read", async () => {
    stubFetch(on("GET", "/api/v1/schedule", { status: 403, body: { error: { code: "forbidden", message: "not your dealership" } } }));
    panel(makeAppointment({ dealershipId: "d2" }), true, null);
    expect(await screen.findByText(/could not be loaded: not your dealership/)).toBeTruthy();
  });
});

describe("customer wording", () => {
  it("lists services by name and duration only; one click swaps a single service", async () => {
    const other = { ...service, id: "s2", name: "Brake check", durationMinutes: 90 };
    const onChange = vi.fn();
    render(<ServicePicker services={[service, other]} selected={["s1"]} isWaiter={false} onChange={onChange} />);
    expect((screen.getByRole("radio", { name: "Oil change (1h)" }) as HTMLInputElement).checked).toBe(true);
    await userEvent.click(screen.getByRole("radio", { name: "Brake check (1h30m)" }));
    expect(onChange).toHaveBeenLastCalledWith(["s2"]);
    expect(screen.queryByText(/express|lubrication|primary/i)).toBeNull();
  });

  it("adds a second service only after 'Add another service', keeping the last one ticked with a reason", async () => {
    const other = { ...service, id: "s2", name: "Brake check", durationMinutes: 90 };
    const onChange = vi.fn();
    render(<ServicePicker services={[service, other]} selected={["s1"]} isWaiter={false} onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: "Add another service" }));
    const oil = screen.getByRole("checkbox", { name: "Oil change (1h)" }) as HTMLInputElement;
    expect(oil.disabled).toBe(true);
    expect(oil.closest("label")?.title).toMatch(/At least one service is needed/);
    await userEvent.click(screen.getByRole("checkbox", { name: "Brake check (1h30m)" }));
    expect(onChange).toHaveBeenLastCalledWith(["s1", "s2"]);
  });

  it("keeps the requirements for staff and asks them to pick the job", () => {
    const { rerender } = render(<ServicePicker services={[service]} selected={[]} isWaiter={false} isStaff onChange={vi.fn()} />);
    expect((screen.getByRole("radio", { name: /Oil change/ }) as HTMLInputElement).checked).toBe(false);
    expect(screen.getByText("Pick the job for this booking.")).toBeTruthy();
    rerender(<ServicePicker services={[service]} selected={["s1"]} isWaiter={false} isStaff onChange={vi.fn()} />);
    expect(screen.getByRole("radio", { name: /Oil change \(1h · express · lubrication L1\+\)/ })).toBeTruthy();
  });

  it("shows free starts without bay and technician counts, and the zone in the heading", () => {
    const grid = makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]);
    const { rerender } = render(<SlotGrid availability={grid} selectedSlot={null} busy={false} onSelect={vi.fn()} onConfirm={vi.fn()} />);
    expect(screen.getByText("Free")).toBeTruthy();
    expect(screen.getByRole("heading", { name: /London time \(BST\)/ })).toBeTruthy();
    rerender(<SlotGrid availability={grid} selectedSlot={null} busy={false} showCapacity onSelect={vi.fn()} onConfirm={vi.fn()} />);
    expect(screen.getByText("1 bay · 1 tech")).toBeTruthy();
  });
});

describe("DayBoard blocks", () => {
  it("draws a short job on one line and the rest with service and technician, plus a legend", () => {
    const { container } = render(
      <DayBoard
        schedule={makeSchedule([
          makeAppointment({ id: "short", startAt: "2026-10-19T08:00:00Z", endAt: "2026-10-19T08:30:00Z" }),
          makeAppointment({ id: "long", startAt: "2026-10-19T12:00:00Z", endAt: "2026-10-19T14:00:00Z", status: Status.InProgress }),
        ])}
        onSelect={vi.fn()}
      />,
    );
    const short = screen.getByTitle(/09:00–09:30/);
    expect(short.className).toContain("compact");
    expect(short.textContent).toBe("09:00 · Confirmed");
    const long = screen.getByTitle(/13:00–15:00/);
    expect(long.className).not.toContain("compact");
    expect(long.textContent).toContain("Taylor");
    // The track is as tall as the day is long (08:00–17:00 = 540 minutes).
    expect(container.querySelector<HTMLElement>(".board-wrap")?.style.getPropertyValue("--board-minutes")).toBe("540");
    const legend = screen.getByRole("list", { name: "Board legend" });
    expect(within(legend).getByText("In progress")).toBeTruthy();
    expect(within(legend).getByText("Lunch")).toBeTruthy();
    expect(screen.getByRole("heading", { name: /London time \(BST\)/ })).toBeTruthy();
  });
});

describe("AuthPanel", () => {
  it("starts empty and signs in a demo account in one click", async () => {
    const login = vi.fn((_url: URL) => ({
      body: { token: "h.p.s", expiresAt: "2099-01-01T00:00:00Z", user: { id: "u", email: "d", name: "Demo Dispatcher", role: Role.Dispatcher } },
    }));
    const fetch = stubFetch(on("POST", "/api/v1/auth/login", login));
    const onAuthed = vi.fn();
    render(<AuthPanel onAuthed={onAuthed} />);
    expect((screen.getByLabelText("Email") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Password") as HTMLInputElement).value).toBe("");
    await userEvent.click(screen.getByRole("button", { name: /^Dispatcher/ }));
    await waitFor(() => expect(onAuthed).toHaveBeenCalledOnce());
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({ email: "dispatcher@scheduler.local", password: "dispatch123" });
  });

  it("offers no demo sign-in in a build without demo accounts", () => {
    render(<AuthPanel onAuthed={vi.fn()} demoAccounts={[]} />);
    expect(screen.queryByText("Or try a demo account:")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Dispatcher/ })).toBeNull();
  });

  it("keeps the email when switching between sign-in and register, and names no role code", async () => {
    render(<AuthPanel onAuthed={vi.fn()} />);
    await userEvent.type(screen.getByLabelText("Email"), "sam@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Register" }));
    expect((screen.getByLabelText("Email") as HTMLInputElement).value).toBe("sam@example.com");
    expect(screen.queryByText(/role 3/)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Login" }));
    expect((screen.getByLabelText("Email") as HTMLInputElement).value).toBe("sam@example.com");
  });
});

describe("VehicleForm", () => {
  it("shows problems next to the form, before and after the server", async () => {
    stubFetch(on("POST", "/api/v1/vehicles", { status: 409, body: { error: { code: "conflict", message: "this VIN is already registered" } } }));
    render(<VehicleForm onCreated={vi.fn()} />);
    await userEvent.type(screen.getByLabelText(/VIN/), "1HGCM82633A00435O");
    await userEvent.type(screen.getByLabelText("Make"), "Honda");
    await userEvent.type(screen.getByLabelText("Model"), "Accord");
    await userEvent.click(screen.getByRole("button", { name: "Add vehicle" }));
    const section = screen.getByRole("heading", { name: "Add a vehicle" }).closest("section")!;
    expect(within(section).getByRole("alert").textContent).toMatch(/never I, O or Q/);

    await userEvent.clear(screen.getByLabelText(/VIN/));
    await userEvent.type(screen.getByLabelText(/VIN/), "1HGCM82633A004352");
    await userEvent.click(screen.getByRole("button", { name: "Add vehicle" }));
    expect(await within(section).findByText("this VIN is already registered")).toBeTruthy();
  });
});

describe("StaffLayout", () => {
  it("keeps the configuration tab mounted while the workshop is shown", async () => {
    const mounts = vi.fn();
    function Admin() {
      useEffect(() => mounts(), []);
      return <p>Admin drafts</p>;
    }
    render(<StaffLayout admin={<Admin />} bar={<p>Bar</p>} board={<p>Board</p>} appointment={null} booking={<p>Booking</p>} />);
    expect(mounts).not.toHaveBeenCalled(); // loads only once opened
    await userEvent.click(screen.getByRole("tab", { name: "Configuration" }));
    expect(screen.getByText("Admin drafts")).toBeTruthy();
    await userEvent.click(screen.getByRole("tab", { name: "Bookings & day board" }));
    await userEvent.click(screen.getByRole("tab", { name: "Configuration" }));
    expect(mounts).toHaveBeenCalledOnce();
    expect(screen.getByText("Board")).toBeTruthy();
  });
});

describe("staff appointment scope", () => {
  it("uses the API's dealership, vehicle and customer filters", () => {
    const p = { dealershipId: "d1", vehicleId: "v1", customerId: "c1" };
    expect(scopeQuery("dealership", p)).toEqual({ dealershipId: "d1" });
    expect(scopeQuery("vehicle", p)).toEqual({ vehicleId: "v1" });
    expect(scopeQuery("customer", p)).toEqual({ customerId: "c1" });
    expect(scopeQuery("customer", { dealershipId: "d1" })).toEqual({ dealershipId: "d1" });
  });
});
