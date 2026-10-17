import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { currentSession } from "./auth";
import { authResponse, deferred, on, otherTab, signIn, stubFetch } from "./testFixtures";
import { AggregateType, EventType, Role, type AuthUser } from "./types";
import { liveSubscription } from "./live";
import { useLiveEvents, type LiveHandlers } from "./useLiveEvents";

/** A WebSocket the test drives; every one opened is kept in `all`. */
class FakeSocket {
  static readonly OPEN = 1;
  static all: FakeSocket[] = [];
  readyState = 1;
  sent: unknown[] = [];
  onclose: ((ev: { code: number }) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  constructor(readonly url: string) {
    FakeSocket.all.push(this);
  }
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {}
  receive(msg: object) {
    act(() => this.onmessage?.({ data: JSON.stringify(msg) }));
  }
}

function handlers() {
  return {
    onAppointment: vi.fn<LiveHandlers["onAppointment"]>(),
    onAvailability: vi.fn<LiveHandlers["onAvailability"]>(),
    onConfig: vi.fn<LiveHandlers["onConfig"]>(),
    onResync: vi.fn<LiveHandlers["onResync"]>(),
    onUnauthorized: vi.fn<LiveHandlers["onUnauthorized"]>(),
    onSessionChanged: vi.fn<() => void>(),
  };
}

const alice: AuthUser = { id: "u1", email: "a@x", name: "Alice", role: Role.Dispatcher, dealershipId: "d1" };
const bob: AuthUser = { id: "u2", email: "b@x", name: "Bob", role: Role.Customer };
const unauthorized = { status: 401, body: { error: { code: "unauthorized", message: "expired" } } };

const ticket = on("POST", "/api/v1/auth/ws-ticket", { body: { ticket: "t1", expiresAt: "2099-01-01T00:00:00Z" } });

beforeEach(() => {
  FakeSocket.all = [];
  vi.stubGlobal("WebSocket", FakeSocket);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("useLiveEvents", () => {
  it("resyncs after a reconnect (the second welcome frame), never on the first", async () => {
    stubFetch(ticket);
    const h = handlers();
    const { result } = renderHook(() => useLiveEvents("tok", "d1", h));
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
    FakeSocket.all[0].receive({ type: EventType.Connected });
    expect(result.current).toBe("open");
    expect(h.onResync).not.toHaveBeenCalled();

    // The socket drops; after the first backoff (about 1 s) a new ticket opens a new socket that
    // welcomes again.
    act(() => FakeSocket.all[0].onclose!({ code: 1006 }));
    expect(result.current).toBe("closed");
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2), { timeout: 3000 });
    FakeSocket.all[1].receive({ type: EventType.Connected });
    expect(h.onResync).toHaveBeenCalledOnce();
    expect(result.current).toBe("open");
  });

  it("dispatches notices to their handlers", async () => {
    stubFetch(ticket);
    const h = handlers();
    renderHook(() => useLiveEvents("tok", "d1", h));
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
    const s = FakeSocket.all[0];
    s.receive({ type: EventType.AppointmentCreated, payload: { id: "a1" } });
    s.receive({ type: EventType.AvailabilityChanged, payload: { dealershipId: "d1" } });
    s.receive({ type: EventType.ConfigUpdated, payload: { entity: AggregateType.Dealership, id: "d1", version: 2 } });
    expect(h.onAppointment).toHaveBeenCalledWith(EventType.AppointmentCreated, { id: "a1" });
    expect(h.onAvailability).toHaveBeenCalledOnce();
    expect(h.onConfig).toHaveBeenCalledOnce();
  });

  it("opens on the initial dealership and re-subscribes, without reconnecting, when it changes", async () => {
    stubFetch(ticket);
    const h = handlers();
    const { rerender } = renderHook((p: { dealership: string }) => useLiveEvents("tok", p.dealership, h), {
      initialProps: { dealership: "d1" },
    });
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
    const s = FakeSocket.all[0];
    expect(new URL(s.url).searchParams.get("dealershipId")).toBe("d1");
    s.receive({ type: EventType.Connected });
    expect(s.sent).toContainEqual({ type: "subscribe", dealershipId: "d1" });

    rerender({ dealership: "d2" });
    expect(s.sent.at(-1)).toEqual({ type: "subscribe", dealershipId: "d2" });
    // "" = every dealership (a group admin viewing another dealership's appointment).
    rerender({ dealership: "" });
    expect(s.sent.at(-1)).toEqual({ type: "subscribe", dealershipId: "" });
    expect(FakeSocket.all).toHaveLength(1);
  });

  it("signs out on a 401 for the ticket (after the refresh failed) and opens no socket", async () => {
    signIn(alice);
    const key = currentSession()!.key;
    stubFetch(on("POST", "/api/v1/auth/ws-ticket", unauthorized), on("POST", "/api/v1/auth/refresh", { status: 503 }));
    const h = handlers();
    const { result } = renderHook(() => useLiveEvents(key, "d1", h));
    await waitFor(() => expect(h.onUnauthorized).toHaveBeenCalledWith(key));
    expect(result.current).toBe("closed");
    expect(FakeSocket.all).toHaveLength(0);
  });

  it("keeps the socket across a token refresh (the same session)", async () => {
    signIn(alice, { token: "tok-0" });
    const key = currentSession()!.key;
    stubFetch(ticket);
    const { rerender } = renderHook(({ k }) => useLiveEvents(k, "d1", handlers()), { initialProps: { k: key } });
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
    act(() => otherTab().signsIn(authResponse(alice, { token: "tok-1" })));
    expect(currentSession()).toMatchObject({ key, token: "tok-1" });
    rerender({ k: currentSession()!.key });
    expect(FakeSocket.all).toHaveLength(1);
  });

  it("ignores a ticket 401 once the tab switched to another session or the socket was torn down", async () => {
    const answer = deferred<{ status: number; body: unknown }>();
    stubFetch(on("POST", "/api/v1/auth/ws-ticket", () => answer.promise));
    signIn(alice);
    const aliceKey = currentSession()!.key;
    const h = handlers();
    const switched = renderHook(() => useLiveEvents(aliceKey, "d1", h));
    // While the ticket request is in flight, another tab's login (another account) is adopted here.
    otherTab().signsIn(authResponse(bob, { token: "tok-bob" }));
    await act(async () => answer.resolve(unauthorized));
    expect(h.onUnauthorized).not.toHaveBeenCalled();
    switched.unmount();

    const late = deferred<{ status: number; body: unknown }>();
    stubFetch(on("POST", "/api/v1/auth/ws-ticket", () => late.promise), on("POST", "/api/v1/auth/refresh", { status: 503 }));
    const bobKey = currentSession()!.key;
    const disposed = renderHook(() => useLiveEvents(bobKey, "d1", h));
    disposed.unmount();
    await act(async () => late.resolve(unauthorized));
    expect(h.onUnauthorized).not.toHaveBeenCalled();
  });

  it("re-reads the user when the hub closes over the session (1008)", async () => {
    stubFetch(ticket);
    const h = handlers();
    renderHook(() => useLiveEvents("tok", "d1", h));
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1));
    act(() => FakeSocket.all[0].onclose!({ code: 1008 }));
    expect(h.onSessionChanged).toHaveBeenCalledOnce();
  });
});

describe("liveSubscription", () => {
  it("follows the form, or every dealership for a group admin viewing another one's appointment", () => {
    expect(liveSubscription("d1", undefined, true)).toBe("d1");
    expect(liveSubscription("d1", "d1", true)).toBe("d1");
    expect(liveSubscription("d1", "d2", true)).toBe("");
    // Dealership staff never see another dealership; customers get their own bookings anyway.
    expect(liveSubscription("d1", "d2", false)).toBe("d1");
  });
});
