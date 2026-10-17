// Shared fixtures for component tests: API-shaped objects and a fetch stub routed by method + path.
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "vitest";
import { startSession } from "./auth";
import { Role, Status, type Appointment, type AuthResponse, type AuthUser, type Availability, type Dealership, type Schedule, type ServiceType } from "./types";

export const dealership: Dealership = {
  id: "d1",
  name: "Northside",
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
  today: "2026-10-19",
  version: 1,
};

export const service: ServiceType = {
  id: "s1",
  code: "OIL",
  name: "Oil change",
  durationMinutes: 60,
  requiredSkill: 1,
  requiredBayType: 1,
  minSkillLevel: 1,
  version: 1,
};

export function makeAppointment(over: Partial<Appointment> = {}): Appointment {
  return {
    id: "a1",
    dealershipId: "d1",
    dealershipName: "Northside",
    customerId: "c1",
    customerName: "Casey",
    vehicleId: "v1",
    vehicleLabel: "Toyota Corolla",
    vin: "JT2BG22K1W0123456",
    serviceTypeId: "s1",
    serviceName: "Oil change",
    durationMinutes: 60,
    lines: [{ lineNo: 1, serviceTypeId: "s1", code: "OIL", name: "Oil change", durationMinutes: 60 }],
    requiredBayType: 1,
    requiredSkills: [1],
    minSkillLevel: 1,
    serviceBayId: "b1",
    serviceBayName: "Express 1",
    serviceBayType: 1,
    technicianId: "t1",
    technicianName: "Taylor",
    // 10:00–11:00 London (BST) on 2026-10-19
    startAt: "2026-10-19T09:00:00Z",
    endAt: "2026-10-19T10:00:00Z",
    status: Status.Confirmed,
    isWaiter: false,
    version: 1,
    ...over,
  };
}

export function makeAvailability(date: string, starts: string[]): Availability {
  return {
    date,
    timezone: "Europe/London",
    durationMinutes: 60,
    slots: starts.map((start) => ({
      start,
      end: new Date(Date.parse(start) + 3_600_000).toISOString(),
      available: true,
      freeBays: 1,
      freeTechnicians: 1,
    })),
  };
}

export function makeSchedule(appointments: Appointment[]): Schedule {
  return {
    date: "2026-10-19",
    timezone: "Europe/London",
    openTime: "08:00",
    closeTime: "17:00",
    bays: [{ id: "b1", name: "Express 1", bayType: 1 }],
    technicians: [{ id: "t1", name: "Taylor", skillLevel: 1, skills: [1] }],
    appointments,
  };
}

export const dispatcher: AuthUser = { id: "u1", email: "d@x", name: "Dana", role: Role.Dispatcher, dealershipId: "d1" };

/**
 * Pins the clock to 08:00 London on the fixture dealership's today (2026-10-19): staff forms and
 * boards start on the dealership's today by the clock. Only Date is faked; timers stay real.
 */
export function pinToday() {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-19T07:00:00Z"));
}

/** Far enough ahead that no refresh or session end happens during a test. */
export const FAR_FUTURE = "2099-01-01T00:00:00Z";

/** What a login answers for `user`. */
export function authResponse(user: AuthUser, over: Partial<AuthResponse> = {}): AuthResponse {
  return { token: "header.payload.sig", expiresAt: FAR_FUTURE, sessionExpiresAt: FAR_FUTURE, user, ...over };
}

/** Starts a session in memory, as a login does (the app then needs no boot refresh). */
export function signIn(user: AuthUser, over: Partial<AuthResponse> = {}) {
  startSession(authResponse(user, over));
}

/**
 * BroadcastChannel stand-in (testSetup installs it): a message posted on one instance reaches
 * every other open instance of the same name synchronously, like another tab of the origin.
 */
export class FakeBroadcastChannel {
  static open: FakeBroadcastChannel[] = [];
  /** Every message posted, by any instance (what the app told the other tabs). */
  static posted: { name: string; data: unknown }[] = [];
  private listeners: ((e: MessageEvent) => void)[] = [];
  onmessage: ((e: MessageEvent) => void) | null = null;
  constructor(readonly name: string) {
    FakeBroadcastChannel.open.push(this);
  }
  postMessage(data: unknown) {
    FakeBroadcastChannel.posted.push({ name: this.name, data });
    for (const other of FakeBroadcastChannel.open) {
      if (other === this || other.name !== this.name) continue;
      const ev = new MessageEvent("message", { data: structuredClone(data) });
      other.onmessage?.(ev);
      for (const l of other.listeners) l(ev);
    }
  }
  addEventListener(_type: "message", l: (e: MessageEvent) => void) {
    this.listeners.push(l);
  }
  removeEventListener(_type: "message", l: (e: MessageEvent) => void) {
    this.listeners = this.listeners.filter((x) => x !== l);
  }
  close() {
    FakeBroadcastChannel.open = FakeBroadcastChannel.open.filter((c) => c !== this);
  }
  static reset() {
    FakeBroadcastChannel.open = [];
    FakeBroadcastChannel.posted = [];
  }
}

/** Another tab of this origin: what it says on the "uss-auth" channel reaches this tab's auth. */
export function otherTab() {
  const ch = new FakeBroadcastChannel("uss-auth");
  return {
    signsIn: (r: AuthResponse) => ch.postMessage({ type: "session", ...r }),
    signsOut: () => ch.postMessage({ type: "signed-out" }),
    close: () => ch.close(),
  };
}

/**
 * Staff pick the car and the job themselves (nothing is preselected for them): choose `vehicleId`
 * in the vehicle select once it is listed, then the service.
 */
export async function pickCarAndJob(vehicleId = "v1", serviceName: RegExp = /^Oil change/) {
  const select = (await screen.findByRole("combobox", { name: "Vehicle" })) as HTMLSelectElement;
  await waitFor(() => expect(Array.from(select.options).some((o) => o.value === vehicleId)).toBe(true));
  await userEvent.selectOptions(select, vehicleId);
  await userEvent.click(screen.getByRole("radio", { name: serviceName }));
}

type Reply = { status?: number; body?: unknown; headers?: Record<string, string> };
type Route = (url: URL, init: RequestInit | undefined) => Reply | Promise<Reply> | undefined;

/** Stubs fetch: the first route returning a reply answers; unknown requests get 404. */
export function stubFetch(...routes: Route[]) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://localhost");
    for (const route of routes) {
      const reply = await route(url, init);
      if (reply) {
        const status = reply.status ?? 200;
        const text = reply.body === undefined ? "" : JSON.stringify(reply.body);
        return new Response(status === 204 ? null : text, { status, headers: reply.headers });
      }
    }
    return new Response(JSON.stringify({ error: { code: "not_found", message: `no stub for ${url.pathname}` } }), {
      status: 404,
    });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

/** A route answering `method path` (path matched exactly, query ignored). */
export function on(method: string, path: string, reply: Reply | ((url: URL) => Reply | Promise<Reply>)): Route {
  return (url, init) => {
    if ((init?.method ?? "GET") !== method || url.pathname !== path) return undefined;
    return typeof reply === "function" ? reply(url) : reply;
  };
}

/** A promise with its resolve function exposed (to finish requests out of order). */
export function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
