import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, refreshSession } from "./api";
import { AUTH_CHANNEL, REFRESH_LOCK, authHeaders, currentSession, startSession } from "./auth";
import { FakeBroadcastChannel, authResponse, on, otherTab, signIn, stubFetch } from "./testFixtures";
import { EXPIRY_WARNING_MS, MIN_REFRESH_GAP_MS, REFRESH_AHEAD_MS, SESSION_CHECK_FAILED, SESSION_EXPIRED, SIGNED_OUT_ELSEWHERE, useSession } from "./useSession";
import { Role, type AuthResponse, type AuthUser } from "./types";

const alice: AuthUser = { id: "u1", email: "a@x", name: "Alice", role: Role.Dispatcher, dealershipId: "d1" };
const bob: AuthUser = { id: "u2", email: "b@x", name: "Bob", role: Role.Customer };
const unauthorized = { status: 401, body: { error: { code: "unauthorized", message: "token expired" } } };

const iso = (ms: number) => new Date(ms).toISOString();

/** A refresh endpoint answering with successive tokens tok-1, tok-2, … for `user`. */
function refreshes(user: AuthUser, over: (n: number) => Partial<AuthResponse> = () => ({})) {
  let n = 0;
  return vi.fn(() => {
    n++;
    return { body: authResponse(user, { token: `tok-${n}`, ...over(n) }) };
  });
}

function bearer(init: RequestInit | undefined): string | null {
  return new Headers(init?.headers).get("Authorization");
}

/** The calls `fetch` got for `path`. */
function callsTo(fetch: ReturnType<typeof stubFetch>, path: string) {
  return fetch.mock.calls.filter(([input]) => new URL(String(input), "http://localhost").pathname === path);
}

/** Every key in either storage (the session must never be written to one). */
function storedKeys(): string[] {
  const keys: string[] = [];
  for (const s of [localStorage, sessionStorage]) for (let i = 0; i < s.length; i++) keys.push(s.key(i)!);
  return keys;
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

describe("boot", () => {
  it("restores the session from the refresh cookie, showing `checking` until it answers", async () => {
    const refresh = refreshes(alice);
    const fetch = stubFetch(on("POST", "/api/v1/auth/refresh", refresh));
    const { result } = renderHook(() => useSession(vi.fn()));
    expect(result.current.checking).toBe(true);
    expect(result.current.session).toBeNull();
    await waitFor(() => expect(result.current.checking).toBe(false));
    expect(result.current.session?.user.name).toBe("Alice");
    expect(result.current.session?.token).toBe("tok-1");
    expect(result.current.notice).toBe("");

    const [, init] = callsTo(fetch, "/api/v1/auth/refresh")[0];
    expect(init?.credentials).toBe("same-origin");
    expect(new Headers(init?.headers).get("X-Requested-With")).toBe("fetch");
    expect(bearer(init)).toBeNull();
    expect(refresh).toHaveBeenCalledOnce();
  });

  it("is signed out, without a notice, when there is no refresh session (401)", async () => {
    stubFetch(on("POST", "/api/v1/auth/refresh", unauthorized));
    const { result } = renderHook(() => useSession(vi.fn()));
    await waitFor(() => expect(result.current.checking).toBe(false));
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe("");
  });

  it("says so when the server could not be reached", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("Failed to fetch"))));
    const { result } = renderHook(() => useSession(vi.fn()));
    await waitFor(() => expect(result.current.checking).toBe(false));
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe(SESSION_CHECK_FAILED);
  });

  it("removes what an older build stored, and never stores the session itself", async () => {
    localStorage.setItem("uss.token", "old-token");
    localStorage.setItem("uss.user", JSON.stringify(alice));
    sessionStorage.setItem("uss.token", "old-token");
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    stubFetch(on("POST", "/api/v1/auth/refresh", refreshes(alice)), on("POST", "/api/v1/auth/login", { body: authResponse(bob, { token: "tok-bob" }) }));
    const { result } = renderHook(() => useSession(vi.fn()));
    expect(storedKeys()).toEqual([]);
    await waitFor(() => expect(result.current.session?.token).toBe("tok-1"));
    await act(async () => {
      await refreshSession();
    });
    act(() => result.current.signIn(authResponse(bob, { token: "tok-bob" })));
    expect(result.current.session?.token).toBe("tok-bob");
    expect(setItem).not.toHaveBeenCalled();
    expect(storedKeys()).toEqual([]);
    setItem.mockRestore();
  });
});

describe("refresh", () => {
  it("refreshes the access token shortly before it expires", async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const refresh = refreshes(alice, () => ({ expiresAt: iso(Date.now() + 15 * 60_000), sessionExpiresAt: iso(now + 86_400_000) }));
    stubFetch(on("POST", "/api/v1/auth/refresh", refresh));
    signIn(alice, { token: "tok-0", expiresAt: iso(now + 5 * 60_000), sessionExpiresAt: iso(now + 86_400_000) });
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    const key = result.current.session?.key;

    await act(() => vi.advanceTimersByTimeAsync(5 * 60_000 - REFRESH_AHEAD_MS - 1_000));
    expect(refresh).not.toHaveBeenCalled();
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(refresh).toHaveBeenCalledOnce();
    expect(result.current.session?.token).toBe("tok-1");
    // The same session (nothing shown is dropped), told to the other tabs.
    expect(result.current.session?.key).toBe(key);
    expect(onSignOut).not.toHaveBeenCalled();
    expect(FakeBroadcastChannel.posted.at(-1)).toEqual({ name: AUTH_CHANNEL, data: expect.objectContaining({ type: "session", token: "tok-1" }) });

    // The next one is scheduled from the new expiry.
    await act(() => vi.advanceTimersByTimeAsync(15 * 60_000 - REFRESH_AHEAD_MS));
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it("refreshes a short-lived token halfway through, never in a loop", async () => {
    vi.useFakeTimers();
    const refresh = refreshes(alice, () => ({ expiresAt: iso(Date.now() + 60_000) }));
    stubFetch(on("POST", "/api/v1/auth/refresh", refresh));
    signIn(alice, { token: "tok-0", expiresAt: iso(Date.now() + 60_000) });
    renderHook(() => useSession(vi.fn()));
    await act(() => vi.advanceTimersByTimeAsync(29_000));
    expect(refresh).not.toHaveBeenCalled();
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(refresh).toHaveBeenCalledOnce();
    // An already expired token (a skewed clock) waits MIN_REFRESH_GAP_MS between attempts.
    refresh.mockImplementation(() => ({ body: authResponse(alice, { token: `tok-${Date.now()}`, expiresAt: iso(Date.now() - 1_000) }) }));
    await act(() => vi.advanceTimersByTimeAsync(30_000 + 5 * MIN_REFRESH_GAP_MS));
    expect(refresh.mock.calls.length).toBeLessThanOrEqual(7);
  });

  it("refreshes on a 401 and retries the request once with the new token", async () => {
    signIn(alice, { token: "tok-0" });
    const me = vi.fn((_url: URL) => ({ body: alice }));
    const fetch = stubFetch(
      (url, init) => (url.pathname === "/api/v1/auth/me" && bearer(init) === "Bearer tok-0" ? unauthorized : undefined),
      on("GET", "/api/v1/auth/me", me),
      on("POST", "/api/v1/auth/refresh", refreshes(alice)),
    );
    await expect(api.me()).resolves.toEqual(alice);
    const meCalls = callsTo(fetch, "/api/v1/auth/me");
    expect(meCalls.map(([, init]) => bearer(init))).toEqual(["Bearer tok-0", "Bearer tok-1"]);
    expect(currentSession()?.token).toBe("tok-1");
  });

  it("shares one refresh between concurrent 401s", async () => {
    signIn(alice, { token: "tok-0" });
    const refresh = refreshes(alice);
    stubFetch(
      (url, init) => (url.pathname !== "/api/v1/auth/refresh" && bearer(init) === "Bearer tok-0" ? unauthorized : undefined),
      on("GET", "/api/v1/auth/me", { body: alice }),
      on("GET", "/api/v1/vehicles/v1", { body: { id: "v1" } }),
      on("POST", "/api/v1/auth/refresh", refresh),
    );
    await Promise.all([api.me(), api.vehicle("v1"), api.me()]);
    expect(refresh).toHaveBeenCalledOnce();
  });

  it("holds the cross-tab lock around a refresh where the browser has Web Locks", async () => {
    const request = vi.fn((_name: string, fn: () => Promise<unknown>) => fn());
    vi.stubGlobal("navigator", { ...navigator, locks: { request } });
    stubFetch(on("POST", "/api/v1/auth/refresh", refreshes(alice)));
    await refreshSession();
    expect(request).toHaveBeenCalledWith(REFRESH_LOCK, expect.any(Function));
  });

  it("takes the session another tab refreshed to while waiting for the lock, without refreshing again", async () => {
    signIn(alice, { token: "tok-0" });
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    vi.stubGlobal("navigator", { ...navigator, locks: { request: async (_n: string, fn: () => Promise<unknown>) => (await held, fn()) } });
    const refresh = refreshes(alice);
    stubFetch(on("POST", "/api/v1/auth/refresh", refresh));
    const pending = refreshSession();
    otherTab().signsIn(authResponse(alice, { token: "tok-other" }));
    release();
    expect((await pending)?.token).toBe("tok-other");
    expect(refresh).not.toHaveBeenCalled();
  });

  it("signs out with the expiry notice when the refresh is refused", async () => {
    signIn(alice, { token: "tok-0" });
    stubFetch(on("GET", "/api/v1/auth/me", unauthorized), on("POST", "/api/v1/auth/refresh", unauthorized));
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    await act(async () => {
      await expect(api.me()).rejects.toMatchObject({ status: 401 });
    });
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe(SESSION_EXPIRED);
    expect(onSignOut).toHaveBeenCalledOnce();
  });

  it("signs out when the retried request is refused too", async () => {
    signIn(alice, { token: "tok-0" });
    const fetch = stubFetch(on("GET", "/api/v1/auth/me", unauthorized), on("POST", "/api/v1/auth/refresh", refreshes(alice)));
    const { result } = renderHook(() => useSession(vi.fn()));
    await act(async () => {
      await expect(api.me()).rejects.toMatchObject({ status: 401 });
    });
    expect(callsTo(fetch, "/api/v1/auth/me")).toHaveLength(2);
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe(SESSION_EXPIRED);
  });

  it("keeps the session when the refresh fails for a transient reason", async () => {
    signIn(alice, { token: "tok-0" });
    stubFetch(on("GET", "/api/v1/auth/me", unauthorized), on("POST", "/api/v1/auth/refresh", { status: 503, body: { error: { code: "busy", message: "busy" } } }));
    await expect(api.me()).rejects.toMatchObject({ status: 401 });
    expect(currentSession()?.token).toBe("tok-0");
  });

  it("never replays a request as another account, nor ends that account's session", async () => {
    signIn(alice, { token: "tok-alice" });
    let answer!: (r: { status: number; body: unknown }) => void;
    const fetch = stubFetch(
      on("GET", "/api/v1/auth/me", () => new Promise((r) => (answer = r))),
      on("POST", "/api/v1/auth/refresh", refreshes(bob)),
    );
    const pending = api.me();
    await waitFor(() => expect(answer).toBeTypeOf("function"));
    otherTab().signsIn(authResponse(bob, { token: "tok-bob" }));
    answer(unauthorized);
    await expect(pending).rejects.toMatchObject({ status: 401 });
    expect(callsTo(fetch, "/api/v1/auth/me")).toHaveLength(1);
    expect(callsTo(fetch, "/api/v1/auth/refresh")).toHaveLength(0);
    expect(currentSession()?.token).toBe("tok-bob");
  });

  it("does not refresh on a failed login", async () => {
    const fetch = stubFetch(on("POST", "/api/v1/auth/login", unauthorized));
    await expect(api.login({ email: "a@x", password: "wrong-pass" })).rejects.toMatchObject({ status: 401 });
    expect(callsTo(fetch, "/api/v1/auth/refresh")).toHaveLength(0);
  });
});

describe("across tabs", () => {
  it("adopts another tab's refresh of the same account silently", () => {
    signIn(alice, { token: "tok-0" });
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    const key = result.current.session?.key;
    act(() => otherTab().signsIn(authResponse({ ...alice, name: "Alice B." }, { token: "tok-1" })));
    expect(result.current.session).toMatchObject({ key, token: "tok-1", user: { name: "Alice B." } });
    expect(result.current.notice).toBe("");
    expect(onSignOut).not.toHaveBeenCalled();
    expect(new Headers(authHeaders()).get("Authorization")).toBe("Bearer tok-1");
  });

  it("switches to another account signed in elsewhere, dropping the old user's state, with a notice", () => {
    signIn(alice, { token: "tok-alice" });
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    const key = result.current.session?.key;
    act(() => otherTab().signsIn(authResponse(bob, { token: "tok-bob" })));
    expect(onSignOut).toHaveBeenCalledOnce();
    expect(result.current.session?.token).toBe("tok-bob");
    expect(result.current.session?.key).not.toBe(key);
    expect(result.current.notice).toMatch(/signed in as Bob in another tab/);
  });

  it("joins a session another tab started while signed out here", async () => {
    stubFetch(on("POST", "/api/v1/auth/refresh", unauthorized));
    const { result } = renderHook(() => useSession(vi.fn()));
    await waitFor(() => expect(result.current.checking).toBe(false));
    act(() => otherTab().signsIn(authResponse(bob, { token: "tok-bob" })));
    expect(result.current.session?.user.name).toBe("Bob");
    expect(result.current.notice).toBe("");
  });

  it("signs out when another tab signed out", () => {
    signIn(alice);
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    act(() => otherTab().signsOut());
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe(SIGNED_OUT_ELSEWHERE);
    expect(onSignOut).toHaveBeenCalledOnce();
    expect(new Headers(authHeaders()).get("Authorization")).toBeNull();
  });

  it("ignores malformed messages", () => {
    signIn(alice, { token: "tok-0" });
    const ch = new FakeBroadcastChannel(AUTH_CHANNEL);
    ch.postMessage({ type: "session", token: "x" });
    ch.postMessage("signed-out");
    expect(currentSession()?.token).toBe("tok-0");
  });

  it("tells the other tabs about a login, and about a logout after revoking it server-side", async () => {
    const fetch = stubFetch(on("POST", "/api/v1/auth/logout", { status: 204 }), on("POST", "/api/v1/auth/refresh", unauthorized));
    const { result } = renderHook(() => useSession(vi.fn()));
    await waitFor(() => expect(result.current.checking).toBe(false));
    act(() => result.current.signIn(authResponse(alice, { token: "tok-a" })));
    expect(FakeBroadcastChannel.posted.at(-1)?.data).toMatchObject({ type: "session", token: "tok-a", user: { id: "u1" } });

    act(() => result.current.logout());
    // Hidden at once (here and, via "signing-out", in the other tabs); confirmed by the 204.
    expect(result.current.session).toBeNull();
    expect(FakeBroadcastChannel.posted.at(-1)?.data).toEqual({ type: "signing-out" });
    await waitFor(() => expect(result.current.signOut).toBeNull());
    const [, init] = callsTo(fetch, "/api/v1/auth/logout")[0];
    expect(bearer(init)).toBe("Bearer tok-a");
    expect(new Headers(init?.headers).get("X-Requested-With")).toBe("fetch");
    expect(init?.credentials).toBe("same-origin");
    expect(FakeBroadcastChannel.posted.at(-1)).toEqual({ name: AUTH_CHANNEL, data: { type: "signed-out" } });
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe("");
  });
});

describe("session end", () => {
  it("warns before the sign-in's absolute end, not the access token's expiry, then signs out", async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const end = Math.floor((now + EXPIRY_WARNING_MS + 5 * 60_000) / 1000) * 1000;
    // Each refresh issues a 2-minute access token; the sign-in itself ends at `end`.
    stubFetch(on("POST", "/api/v1/auth/refresh", refreshes(alice, () => ({ expiresAt: iso(Date.now() + 2 * 60_000), sessionExpiresAt: iso(end) }))));
    startSession(authResponse(alice, { token: "tok-0", expiresAt: iso(now + 2 * 60_000), sessionExpiresAt: iso(end) }));
    const { result } = renderHook(() => useSession(vi.fn()));

    await act(() => vi.advanceTimersByTimeAsync(3 * 60_000));
    expect(result.current.session?.token).not.toBe("tok-0");
    expect(result.current.expiresAt).toBeNull();
    await act(() => vi.advanceTimersByTimeAsync(end - EXPIRY_WARNING_MS - Date.now()));
    expect(result.current.expiresAt).toBe(end);
    await act(() => vi.advanceTimersByTimeAsync(EXPIRY_WARNING_MS));
    expect(result.current.session).toBeNull();
    expect(result.current.notice).toBe(SESSION_EXPIRED);
  });
});
