import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { resetAuthForTests } from "./auth";
import {
  authResponse,
  deferred,
  otherTab,
  dealership,
  dispatcher,
  makeAppointment,
  makeAvailability,
  makeSchedule,
  on,
  pickCarAndJob,
  pinToday,
  service,
  signIn,
  stubFetch,
} from "./testFixtures";
import { Role, Status, type AuthUser, type Vehicle } from "./types";

// The dispatcher's shop: one dealership, one service, no vehicles, a live socket that never opens.
const shop = [
  on("GET", "/api/v1/auth/me", { body: dispatcher }),
  on("GET", "/api/v1/dealerships", { body: [dealership] }),
  on("GET", "/api/v1/service-types", { body: [service] }),
  on("GET", "/api/v1/vehicles", { body: [] }),
  on("GET", "/api/v1/appointments", { body: [] }),
  on("POST", "/api/v1/auth/ws-ticket", { status: 503, body: { error: { code: "busy", message: "busy" } } }),
];

beforeEach(() => {
  pinToday();
  localStorage.clear();
  signIn(dispatcher);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("App", () => {
  it("signs out with a notice when the server rejects the session mid-use", async () => {
    const revoked = { status: 401, body: { error: { code: "unauthorized", message: "token revoked" } } };
    const refresh = vi.fn(() => revoked);
    stubFetch(on("GET", "/api/v1/schedule", revoked), on("POST", "/api/v1/auth/refresh", refresh), ...shop);
    render(<App />);
    expect(await screen.findByText("Your session has expired — please sign in again.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeTruthy();
    // One refresh was tried first (the refresh cookie is revoked too).
    expect(refresh).toHaveBeenCalled();
  });

  it("reloads the appointment and explains a 412 instead of overwriting", async () => {
    // On the appointment's day (check-in opens then); only Date is faked, timers stay real.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-19T08:30:00Z"));
    const v1 = makeAppointment();
    const v2 = makeAppointment({ version: 2, status: Status.CheckedIn });
    const checkIn = vi.fn(() => ({
      status: 412,
      body: { error: { code: "precondition_failed", message: "stale version" } },
    }));
    stubFetch(
      on("GET", "/api/v1/schedule", { body: makeSchedule([v1]) }),
      on("POST", "/api/v1/appointments/a1/check-in", checkIn),
      on("GET", "/api/v1/appointments/a1", { body: v2 }),
      ...shop,
    );
    render(<App />);
    const block = await screen.findByTitle(/Confirmed · 10:00–11:00/);
    await userEvent.click(block);
    const panel = screen.getByRole("heading", { name: "Appointment" }).closest("section")!;
    await userEvent.click(within(panel).getByRole("button", { name: "Check in" }));

    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      expect.stringContaining("just changed by someone else"),
    );
    expect(checkIn).toHaveBeenCalledOnce();
    expect(within(panel).getByText("v2")).toBeTruthy();
    expect(within(panel).getByText("Checked in")).toBeTruthy();
  });

  it("opens an appointment from the board without touching the booking form", async () => {
    stubFetch(on("GET", "/api/v1/schedule", { body: makeSchedule([makeAppointment({ isWaiter: true })]) }), ...shop);
    render(<App />);
    await userEvent.click(await screen.findByTitle(/Confirmed · 10:00–11:00/));
    expect(screen.getByRole("heading", { name: "Appointment" })).toBeTruthy();
    expect((screen.getByRole("checkbox", { name: /Waiter/ }) as HTMLInputElement).checked).toBe(false);
  });

  it("leaves nothing of the last user's form to the next one", async () => {
    const car = (id: string, customerName: string): Vehicle => ({
      id, customerId: `c-${id}`, customerName, vin: `VIN-${id}`, make: "Toyota", model: id, year: 2020,
    });
    const customer: AuthUser = { id: "u2", email: "c@x", name: "Casey", role: Role.Customer };
    let who: AuthUser = dispatcher;
    stubFetch(
      on("GET", "/api/v1/auth/me", () => ({ body: who })),
      on("POST", "/api/v1/auth/logout", { status: 204 }),
      on("POST", "/api/v1/auth/login", () => {
        who = customer;
        return { body: { token: "header.payload2.sig", expiresAt: "2099-01-01T00:00:00Z", user: customer } };
      }),
      on("GET", "/api/v1/vehicles", () => ({ body: who === dispatcher ? [car("staffpick", "Walk-in")] : [car("mine", "Casey")] })),
      on("GET", "/api/v1/schedule", { body: makeSchedule([]) }),
      on("GET", "/api/v1/availability/next", { body: { timezone: "Europe/London", durationMinutes: 60, searchedFrom: "2026-10-19", searchedTo: "2026-11-02", slots: [] } }),
      ...shop,
    );
    render(<App />);
    const vehicle = () => screen.getByRole("combobox", { name: "Vehicle" }) as HTMLSelectElement;
    // Staff book for anyone: neither a car nor a job is picked for them.
    await waitFor(() => expect(screen.getByRole("option", { name: "Choose the customer's car…" })).toBeTruthy());
    expect(vehicle().value).toBe("");
    expect((screen.getByRole("radio", { name: /^Oil change/ }) as HTMLInputElement).checked).toBe(false);
    await pickCarAndJob("staffpick");
    expect(vehicle().value).toBe("staffpick");

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await userEvent.click(await screen.findByRole("button", { name: /^Customer/ }));

    // A customer with exactly one car gets it, and the first service, picked.
    await waitFor(() => expect(vehicle().value).toBe("mine"));
    expect((screen.getByRole("radio", { name: /^Oil change/ }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByText("Selected vehicle…")).toBeNull();
  });

  it("offers a dispatcher only their own dealership and shows a board that fails to load", async () => {
    const other = { ...dealership, id: "d2", name: "Riverside" };
    const schedule = vi.fn(() => ({ status: 500, body: { error: { code: "internal", message: "database down" } } }));
    stubFetch(on("GET", "/api/v1/dealerships", { body: [dealership, other] }), on("GET", "/api/v1/schedule", schedule), ...shop);
    render(<App />);
    expect(await screen.findByText(/Could not load the day board: database down/)).toBeTruthy();
    const select = screen.getByRole("combobox", { name: "Dealership" }) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([expect.stringContaining("Northside")]);

    schedule.mockReturnValue({ status: 200, body: makeSchedule([]) } as never);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { name: /Day board · Mon, 19 Oct 2026 · 08:00–17:00/ })).toBeTruthy();
  });

  it("moves the board between days without touching the grid or the picked slot", async () => {
    // "Today" follows the clock in the dealership's zone: pinned to the fixture's today (beforeEach).
    const days: string[] = [];
    stubFetch(
      on("GET", "/api/v1/schedule", (url) => ({ body: { ...makeSchedule([]), date: url.searchParams.get("date") ?? "" } })),
      on("GET", "/api/v1/vehicles", { body: [{ id: "v1", customerId: "c1", customerName: "Casey", vin: "V", make: "Toyota", model: "Yaris", year: 2020 }] }),
      on("GET", "/api/v1/availability", (url) => {
        const day = url.searchParams.get("date") ?? "";
        days.push(day);
        return { body: makeAvailability(day, [`${day}T09:00:00Z`]) };
      }),
      ...shop,
    );
    render(<App />);
    await pickCarAndJob();
    const check = await screen.findByRole("button", { name: "Check availability" });
    await waitFor(() => expect((check as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(check);
    await screen.findByRole("heading", { name: /Slots.*Mon, 19 Oct 2026/ });
    await userEvent.click(screen.getByRole("button", { name: /^10:00/ }));
    // What Confirm books, in words: car, VIN end and owner, the job, dealership, time, drop-off.
    expect(screen.getByText(/You are booking: 2020 Toyota Yaris \(VIN V, Casey\) · Oil change · Northside · Mon, 19 Oct 2026 10:00 BST · drop-off/)).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "Next day" }));

    expect(await screen.findByRole("heading", { name: /Day board · Tue, 20 Oct 2026/ })).toBeTruthy();
    expect(screen.getByRole("heading", { name: /Slots.*Mon, 19 Oct 2026/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Confirm booking · Mon, 19 Oct 2026 10:00/ })).toBeTruthy();
    expect(days).toEqual(["2026-10-19"]);
    await userEvent.click(screen.getByRole("button", { name: "Today" }));
    expect(await screen.findByRole("heading", { name: /Day board · Mon, 19 Oct 2026/ })).toBeTruthy();
  });

  it("reschedules with the form's waiter flag and locks the car and services meanwhile", async () => {
    const appt = makeAppointment({ startAt: "2030-01-07T10:00:00Z", endAt: "2030-01-07T11:00:00Z" });
    const bodies: unknown[] = [];
    stubFetch(
      on("GET", "/api/v1/schedule", { body: makeSchedule([appt]) }),
      on("GET", "/api/v1/availability", (url) => ({
        body: makeAvailability(url.searchParams.get("date") ?? "", ["2030-01-07T10:00:00Z", "2030-01-07T12:00:00Z"]),
      })),
      on("GET", "/api/v1/vehicles/v1", { body: { id: "v1", customerId: "c1", customerName: "Casey", vin: "V", make: "Toyota", model: "Corolla", year: 2020 } }),
      (url, init) => {
        if (url.pathname !== "/api/v1/appointments/a1/reschedule") return undefined;
        bodies.push(JSON.parse(String(init?.body)));
        return { body: { ...appt, version: 2, startAt: "2030-01-07T12:00:00Z", endAt: "2030-01-07T13:00:00Z", isWaiter: true } };
      },
      ...shop,
    );
    render(<App />);
    await userEvent.click(await screen.findByTitle(/Confirmed/));
    await userEvent.click(screen.getByRole("button", { name: "Reschedule…" }));
    await screen.findByRole("button", { name: /^12:00/ });

    expect((screen.getByRole("combobox", { name: "Vehicle" }) as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByRole("combobox", { name: "Dealership" }) as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByRole("radio", { name: /Oil change/ }) as HTMLInputElement).disabled).toBe(true);

    await userEvent.click(screen.getByRole("checkbox", { name: /Waiter/ }));
    await userEvent.click(screen.getByRole("button", { name: "Check availability" }));
    await userEvent.click(await screen.findByRole("button", { name: /^12:00/ }));
    await userEvent.click(screen.getByRole("button", { name: /^Reschedule to / }));

    await waitFor(() => expect(bodies).toEqual([{ startAt: "2030-01-07T12:00:00Z", isWaiter: true }]));
  });
});

describe("App session", () => {
  const board = on("GET", "/api/v1/schedule", { body: makeSchedule([]) });

  it("checks the session before showing either the app or the login form", async () => {
    resetAuthForTests();
    const answer = deferred<{ body: unknown }>();
    stubFetch(on("POST", "/api/v1/auth/refresh", () => answer.promise), board, ...shop);
    render(<App />);
    expect(screen.getByText("Checking your session…")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
    await act(async () => answer.resolve({ body: authResponse(dispatcher) }));
    expect(await screen.findByRole("button", { name: "Sign out" })).toBeTruthy();
  });

  it("shows the login form when the refresh cookie is gone", async () => {
    resetAuthForTests();
    stubFetch(on("POST", "/api/v1/auth/refresh", { status: 401, body: { error: { code: "unauthorized", message: "no session" } } }), ...shop);
    render(<App />);
    expect(await screen.findByRole("button", { name: "Sign in" })).toBeTruthy();
    expect(screen.queryByText(/session has expired/)).toBeNull();
  });

  it("shows no data and no clean sign-out while the server has not confirmed the sign-out", async () => {
    const logout = vi.fn(() => ({ status: 503, body: { error: { code: "busy", message: "busy" } } }));
    stubFetch(on("POST", "/api/v1/auth/logout", logout), board, ...shop);
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(screen.queryByText(/Dana/)).toBeNull();
    expect(await screen.findByText(/Sign-out not confirmed/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeTruthy();
    // The next retry (1 s later) is confirmed: a clean sign-out.
    logout.mockReturnValue({ status: 204 } as never);
    await waitFor(() => expect(screen.queryByText(/Sign-out not confirmed/)).toBeNull(), { timeout: 3_000 });
    expect(logout).toHaveBeenCalledTimes(2);
  });

  it("keeps the view (no reload) when the access token is refreshed", async () => {
    const me = vi.fn(() => ({ body: dispatcher }));
    stubFetch(on("GET", "/api/v1/auth/me", me), board, ...shop);
    render(<App />);
    expect(await screen.findByRole("button", { name: "Sign out" })).toBeTruthy();
    await waitFor(() => expect(me).toHaveBeenCalledOnce());
    act(() => otherTab().signsIn(authResponse(dispatcher, { token: "header.payload3.sig" })));
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.queryByText(/in another tab/)).toBeNull();
    expect(me).toHaveBeenCalledOnce();
  });
});
