// App flows from the UI review: refused bookings re-read the grid, customers open on bookable
// slots, the live socket's session close re-reads the user, and staff list a walk-in's bookings.
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { dealership, dispatcher, makeAppointment, makeAvailability, makeSchedule, on, pickCarAndJob, pinToday, service, signIn, stubFetch } from "./testFixtures";
import { AggregateType, EventType, Role, type AuthUser, type Vehicle } from "./types";

const car: Vehicle = { id: "v1", customerId: "c1", customerName: "Casey", vin: "V", make: "Toyota", model: "Yaris", year: 2020 };

const shop = [
  on("GET", "/api/v1/dealerships", { body: [dealership] }),
  on("GET", "/api/v1/service-types", { body: [service] }),
  on("GET", "/api/v1/vehicles", { body: [car] }),
  on("GET", "/api/v1/schedule", { body: makeSchedule([]) }),
  on("POST", "/api/v1/auth/ws-ticket", { status: 503, body: { error: { code: "busy", message: "busy" } } }),
];

beforeEach(() => {
  pinToday();
  localStorage.clear();
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("App booking conflicts", () => {
  it("re-reads the grid with the pick dropped and explains a slot taken meanwhile", async () => {
    signIn(dispatcher);
    const grids: string[] = [];
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: dispatcher }),
      on("GET", "/api/v1/appointments", { body: [] }),
      on("GET", "/api/v1/availability", (url) => {
        grids.push(url.searchParams.get("date") ?? "");
        return { body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z", "2026-10-19T10:00:00Z"]) };
      }),
      on("POST", "/api/v1/appointments", {
        status: 409,
        body: { error: { code: "conflict", message: "resources were booked by a concurrent request" } },
      }),
      ...shop,
    );
    render(<App />);
    await pickCarAndJob();
    const check = await screen.findByRole("button", { name: "Check availability" });
    await waitFor(() => expect((check as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(check);
    await userEvent.click(await screen.findByRole("button", { name: /^10:00/ }));
    await userEvent.click(screen.getByRole("button", { name: /^Confirm booking · / }));

    expect((await screen.findByRole("alert")).textContent).toContain(
      "That time was just taken — slots refreshed, please pick another time.",
    );
    await waitFor(() => expect(grids).toHaveLength(2));
    expect(screen.getByRole("button", { name: "Confirm booking" })).toHaveProperty("disabled", true);
  });
});

describe("App customer start", () => {
  it("opens on the earliest free starts and that day's grid, in plain words", async () => {
    const customer: AuthUser = { id: "u2", email: "c@x", name: "Casey", role: Role.Customer, customerId: "c1" };
    signIn(customer);
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: customer }),
      on("GET", "/api/v1/appointments", { body: [] }),
      on("GET", "/api/v1/availability/next", {
        body: {
          timezone: "Europe/London",
          durationMinutes: 60,
          searchedFrom: "2026-10-19",
          searchedTo: "2026-11-02",
          slots: [{ date: "2026-10-21", start: "2026-10-21T08:00:00Z", end: "2026-10-21T09:00:00Z", freeBays: 2, freeTechnicians: 3 }],
        },
      }),
      on("GET", "/api/v1/availability", (url) => ({
        body: makeAvailability(url.searchParams.get("date") ?? "", ["2026-10-21T08:00:00Z"]),
      })),
      ...shop,
    );
    render(<App />);
    expect(await screen.findByRole("heading", { name: /Slots.*Wed, 21 Oct 2026/ })).toBeTruthy();
    expect((screen.getByLabelText("Date") as HTMLInputElement).value).toBe("2026-10-21");
    expect(screen.getByRole("checkbox", { name: "I'll wait at the dealership while it's done" })).toBeTruthy();
    expect(screen.queryByText(/bays? ·/)).toBeNull();
    expect(screen.getByRole("heading", { name: "My appointments" })).toBeTruthy();
  });
});

/** A WebSocket the test drives: the latest one opened is `FakeSocket.last`. */
class FakeSocket {
  static readonly OPEN = 1;
  static last: FakeSocket | null = null;
  readyState = 1;
  onclose: ((ev: { code: number }) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  constructor() {
    FakeSocket.last = this;
  }
  send() {}
  close() {}
}

describe("App live session", () => {
  it("re-reads the user when the socket is closed over the session and follows a new role", async () => {
    signIn(dispatcher);
    let me: AuthUser = dispatcher;
    const meCalls = vi.fn(() => ({ body: me }));
    vi.stubGlobal("WebSocket", FakeSocket);
    stubFetch(
      on("GET", "/api/v1/auth/me", meCalls),
      on("GET", "/api/v1/appointments", { body: [] }),
      on("POST", "/api/v1/auth/ws-ticket", { body: { ticket: "t", expiresAt: "2099-01-01T00:00:00Z" } }),
      on("GET", "/api/v1/admin/technicians", { body: [] }),
      ...shop,
    );
    render(<App />);
    await waitFor(() => expect(FakeSocket.last).not.toBeNull());
    expect(screen.queryByRole("tab", { name: "Configuration" })).toBeNull();

    me = { ...dispatcher, role: Role.Admin };
    act(() => FakeSocket.last!.onclose!({ code: 1008 }));

    expect(await screen.findByRole("tab", { name: "Configuration" })).toBeTruthy();
    expect(meCalls).toHaveBeenCalledTimes(2);
    expect(screen.getByText("Your access changed — the view was reloaded.")).toBeTruthy();
  });
});

describe("App live availability", () => {
  it("re-reads the grid and the first-available list on config or roster changes, keeping the pick", async () => {
    const customer: AuthUser = { id: "u2", email: "c@x", name: "Casey", role: Role.Customer, customerId: "c1" };
    signIn(customer);
    vi.stubGlobal("WebSocket", FakeSocket);
    const reads = { grid: 0, next: 0 };
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: customer }),
      on("GET", "/api/v1/appointments", { body: [] }),
      on("POST", "/api/v1/auth/ws-ticket", { body: { ticket: "t", expiresAt: "2099-01-01T00:00:00Z" } }),
      on("GET", "/api/v1/availability/next", () => {
        reads.next++;
        return {
          body: {
            timezone: "Europe/London",
            durationMinutes: 60,
            searchedFrom: "2026-10-19",
            searchedTo: "2026-11-02",
            slots: [{ date: "2026-10-21", start: "2026-10-21T08:00:00Z", end: "2026-10-21T09:00:00Z", freeBays: 1, freeTechnicians: 1 }],
          },
        };
      }),
      on("GET", "/api/v1/availability", (url) => {
        reads.grid++;
        return { body: makeAvailability(url.searchParams.get("date") ?? "", ["2026-10-21T08:00:00Z", "2026-10-21T09:00:00Z"]) };
      }),
      ...shop,
    );
    render(<App />);
    await userEvent.click(await screen.findByRole("button", { name: "10:00, available" }));
    await waitFor(() => expect(FakeSocket.last?.onmessage).toBeTruthy());
    const send = (msg: object) => act(() => FakeSocket.last!.onmessage!({ data: JSON.stringify(msg) }));
    expect(reads).toEqual({ grid: 1, next: 1 });

    send({ type: EventType.ConfigUpdated, payload: { entity: AggregateType.Dealership, id: "d1", version: 2 } });
    await waitFor(() => expect(reads).toEqual({ grid: 2, next: 2 }));

    send({ type: EventType.AvailabilityChanged, payload: { dealershipId: "d1" } }); // a technician changed
    await waitFor(() => expect(reads).toEqual({ grid: 3, next: 3 }));

    // A booking on the grid's day re-reads the grid and the list; one on another day the list only.
    send({ type: EventType.AvailabilityChanged, payload: { dealershipId: "d1", startAt: "2026-10-21T08:00:00Z", endAt: "2026-10-21T09:00:00Z" } });
    await waitFor(() => expect(reads).toEqual({ grid: 4, next: 4 }));
    send({ type: EventType.AvailabilityChanged, payload: { dealershipId: "d1", startAt: "2026-10-23T08:00:00Z", endAt: "2026-10-23T09:00:00Z" } });
    await waitFor(() => expect(reads).toEqual({ grid: 4, next: 5 }));
    send({ type: EventType.ConfigUpdated, payload: { entity: AggregateType.Dealership, id: "d2", version: 2 } });
    send({ type: EventType.AvailabilityChanged, payload: { dealershipId: "d2" } });
    await new Promise((r) => setTimeout(r, 400));
    expect(reads).toEqual({ grid: 4, next: 5 }); // other dealerships: nothing
    expect(screen.getByRole("button", { name: "10:00, available" }).getAttribute("aria-pressed")).toBe("true");
  });
});

describe("App staff appointment list", () => {
  it("lists a walk-in customer's bookings once looked up, upcoming apart from past", async () => {
    signIn(dispatcher);
    const queries: string[] = [];
    const future = makeAppointment({ id: "next", startAt: "2030-01-07T10:00:00Z", endAt: "2030-01-07T11:00:00Z", customerName: "Ann" });
    const old = makeAppointment({ id: "old", startAt: "2020-01-07T10:00:00Z", endAt: "2020-01-07T11:00:00Z", customerName: "Ann" });
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: dispatcher }),
      on("GET", "/api/v1/customers", { body: [{ id: "c9", name: "Ann", phone: "0400 111 222", email: "" }] }),
      on("GET", "/api/v1/appointments", (url) => {
        queries.push(url.search);
        if (url.searchParams.get("customerId") !== "c9") return { body: [] };
        return { body: url.searchParams.get("when") === "upcoming" ? [future] : [old] };
      }),
      ...shop,
    );
    render(<App />);
    expect(await screen.findByText("No appointments at this dealership yet.")).toBeTruthy();
    // Upcoming and past are two listings of the server, each paged on its own.
    expect([...queries].sort()).toEqual(["?dealershipId=d1&when=past&limit=20", "?dealershipId=d1&when=upcoming&limit=20"]);

    await userEvent.click(screen.getByText("Walk-in: find or add the customer, then their car"));
    await userEvent.type(screen.getByLabelText(/Find the customer/), "Ann");
    await userEvent.click(await screen.findByRole("button", { name: /Ann/ }));

    const list = screen.getByRole("heading", { name: "Appointments" }).closest("section")!;
    await waitFor(() => expect(within(list).getAllByRole("button", { pressed: false })).toHaveLength(2));
    expect(queries.slice(-2).sort()).toEqual(["?customerId=c9&when=past&limit=20", "?customerId=c9&when=upcoming&limit=20"]);
    expect((within(list).getByRole("combobox", { name: "Show" }) as HTMLSelectElement).value).toBe("customer");
    expect(within(list).getByText("Past and cancelled (1)")).toBeTruthy();
  });
});
