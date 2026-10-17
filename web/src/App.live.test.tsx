// App flows from the second review: a late re-read never replaces the appointment opened since,
// a booking whose outcome is unknown (503) keeps its Idempotency-Key, and focus follows a booking.
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import {
  dealership,
  deferred,
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
import { EventType, Status, type Appointment, type Vehicle } from "./types";

const car: Vehicle = { id: "v1", customerId: "c1", customerName: "Casey", vin: "V", make: "Toyota", model: "Yaris", year: 2020 };

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

beforeEach(() => {
  pinToday();
  localStorage.clear();
  signIn(dispatcher);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("App appointment panel", () => {
  it("keeps the appointment opened since when a live re-read of the previous one arrives late", async () => {
    const a1 = makeAppointment();
    const a2 = makeAppointment({ id: "a2", customerName: "Ann", startAt: "2026-10-19T11:00:00Z", endAt: "2026-10-19T12:00:00Z" });
    const slow = deferred<{ body: Appointment }>();
    vi.stubGlobal("WebSocket", FakeSocket);
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: dispatcher }),
      on("GET", "/api/v1/dealerships", { body: [dealership] }),
      on("GET", "/api/v1/service-types", { body: [service] }),
      on("GET", "/api/v1/vehicles", { body: [] }),
      on("GET", "/api/v1/appointments", { body: [] }),
      on("POST", "/api/v1/auth/ws-ticket", { body: { ticket: "t", expiresAt: "2099-01-01T00:00:00Z" } }),
      on("GET", "/api/v1/schedule", { body: makeSchedule([a1, a2]) }),
      on("GET", "/api/v1/appointments/a1", () => slow.promise),
    );
    render(<App />);
    await userEvent.click(await screen.findByTitle(/Confirmed · 10:00–11:00/));
    await waitFor(() => expect(FakeSocket.last?.onmessage).toBeTruthy());
    act(() =>
      FakeSocket.last!.onmessage!({
        data: JSON.stringify({ type: EventType.AppointmentCheckedIn, payload: { ...a1, status: Status.CheckedIn, version: 2 } }),
      }),
    );
    // Before the re-read of a1 answers, the user opens a2.
    await userEvent.click(screen.getByTitle(/Confirmed · 12:00–13:00/));
    await act(async () => slow.resolve({ body: { ...a1, version: 2, status: Status.CheckedIn } }));

    const panel = screen.getByRole("heading", { name: "Appointment" }).closest("section")!;
    expect(within(panel).getByText("Ann")).toBeTruthy();
    expect(within(panel).queryByText("Checked in")).toBeNull();
  });
});

describe("App booking with an unknown outcome", () => {
  it("re-reads the grid and lists silently after a 503, keeps the key through a 429, then focuses the booked appointment", async () => {
    const keys: (string | null)[] = [];
    const reads = { grid: 0, lists: 0 };
    // 503 (maybe saved), then 429 (throttled, not judged), then the replay.
    const answers = [
      { status: 503, body: { error: { code: "busy", message: "deadline" } } },
      { status: 429, body: { error: { code: "rate_limited", message: "slow down" } }, headers: { "Retry-After": "1" } },
    ];
    stubFetch(
      on("GET", "/api/v1/auth/me", { body: dispatcher }),
      on("GET", "/api/v1/dealerships", { body: [dealership] }),
      on("GET", "/api/v1/service-types", { body: [service] }),
      on("GET", "/api/v1/vehicles", { body: [car] }),
      on("GET", "/api/v1/schedule", { body: makeSchedule([]) }),
      on("POST", "/api/v1/auth/ws-ticket", { status: 503, body: { error: { code: "busy", message: "busy" } } }),
      on("GET", "/api/v1/appointments", () => {
        reads.lists++;
        return { body: [] };
      }),
      on("GET", "/api/v1/availability", () => {
        reads.grid++;
        return { body: makeAvailability("2026-10-19", ["2026-10-19T09:00:00Z"]) };
      }),
      (url, init) => {
        if (url.pathname !== "/api/v1/appointments" || init?.method !== "POST") return undefined;
        keys.push(new Headers(init.headers).get("Idempotency-Key"));
        return answers.shift() ?? { status: 201, body: makeAppointment() };
      },
    );
    render(<App />);
    await pickCarAndJob();
    const check = screen.getByRole("button", { name: "Check availability" });
    await waitFor(() => expect((check as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(check);
    await userEvent.click(await screen.findByRole("button", { name: /^10:00/ }));
    const listsBefore = reads.lists;
    await userEvent.click(screen.getByRole("button", { name: /^Confirm booking · / }));

    expect((await screen.findByRole("alert")).textContent).toMatch(/busy right now/);
    await waitFor(() => expect(reads.grid).toBe(2));
    await waitFor(() => expect(reads.lists).toBeGreaterThan(listsBefore));
    // The slot is still offered: it stays picked, and each retry replays the same request.
    await userEvent.click(screen.getByRole("button", { name: /^Confirm booking · / }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/Too many requests/);
    await userEvent.click(screen.getByRole("button", { name: /^Confirm booking · / }));
    await waitFor(() => expect(keys).toHaveLength(3));
    expect(new Set(keys).size).toBe(1);
    await waitFor(() => expect(document.activeElement?.textContent).toBe("Appointment"));
  });
});
