// Sign-out that the server must confirm, and refreshes that must not fall outside the server's
// reuse grace (security review): see useSession.ts and api.ts.
import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, refreshSession, SESSION_REQUEST_TIMEOUT_MS } from "./api";
import { AUTH_CHANNEL, SIGN_OUT_PENDING_KEY, currentSession, resetAuthForTests } from "./auth";
import { FakeBroadcastChannel, authResponse, on, otherTab, signIn, stubFetch } from "./testFixtures";
import { SIGNED_OUT_ELSEWHERE, SIGN_OUT_RETRY_DELAYS_MS, useSession } from "./useSession";
import { Role, type AuthUser } from "./types";

const alice: AuthUser = { id: "u1", email: "a@x", name: "Alice", role: Role.Dispatcher, dealershipId: "d1" };
const busy = { status: 503, body: { error: { code: "busy", message: "busy" } } };

function callsTo(fetch: ReturnType<typeof stubFetch>, path: string) {
  return fetch.mock.calls.filter(([input]) => new URL(String(input), "http://localhost").pathname === path);
}

const posted = () => FakeBroadcastChannel.posted.filter((p) => p.name === AUTH_CHANNEL).map((p) => p.data);

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

describe("sign-out confirmed by the server", () => {
  it("hides the session at once but says signed out only after the server confirms, retrying meanwhile", async () => {
    vi.useFakeTimers();
    signIn(alice, { token: "tok-a" });
    const logout = vi.fn().mockReturnValueOnce(busy).mockReturnValueOnce(busy).mockReturnValue({ status: 204 });
    const fetch = stubFetch(on("POST", "/api/v1/auth/logout", logout));
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));

    act(() => result.current.logout());
    expect(result.current.session).toBeNull();
    expect(currentSession()).toBeNull();
    expect(onSignOut).toHaveBeenCalledOnce();
    expect(posted().at(-1)).toEqual({ type: "signing-out" });
    expect(sessionStorage.getItem(SIGN_OUT_PENDING_KEY)).toBe("1");

    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(result.current.signOut).toBe("retrying");
    expect(posted()).not.toContainEqual({ type: "signed-out" });
    await act(() => vi.advanceTimersByTimeAsync(SIGN_OUT_RETRY_DELAYS_MS[2]));
    expect(logout).toHaveBeenCalledTimes(3);
    expect(result.current.signOut).toBeNull();
    expect(result.current.notice).toBe("");
    expect(posted().at(-1)).toEqual({ type: "signed-out" });
    expect(sessionStorage.getItem(SIGN_OUT_PENDING_KEY)).toBeNull();
    // Every attempt carried the Bearer token it was signed in with (and the cookie, same-origin).
    expect(callsTo(fetch, "/api/v1/auth/logout").map(([, i]) => new Headers(i?.headers).get("Authorization"))).toEqual(Array(3).fill("Bearer tok-a"));
    // No storage ever held the token.
    expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain("tok-a");
  });

  it("takes a 401 as confirmed (nothing left to revoke)", async () => {
    signIn(alice);
    stubFetch(on("POST", "/api/v1/auth/logout", { status: 401, body: { error: { code: "unauthorized", message: "no session" } } }));
    const { result } = renderHook(() => useSession(vi.fn()));
    act(() => result.current.logout());
    await waitFor(() => expect(result.current.signOut).toBeNull());
    expect(posted().at(-1)).toEqual({ type: "signed-out" });
  });

  it("gives up after a round, then retries on demand and when back online", async () => {
    vi.useFakeTimers();
    signIn(alice);
    const logout = vi.fn(() => busy);
    stubFetch(on("POST", "/api/v1/auth/logout", logout));
    const { result } = renderHook(() => useSession(vi.fn()));
    act(() => result.current.logout());
    await act(() => vi.advanceTimersByTimeAsync(SIGN_OUT_RETRY_DELAYS_MS.at(-1)! + 1));
    expect(logout).toHaveBeenCalledTimes(SIGN_OUT_RETRY_DELAYS_MS.length);
    expect(result.current.signOut).toBe("failed");
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(logout).toHaveBeenCalledTimes(SIGN_OUT_RETRY_DELAYS_MS.length);

    act(() => result.current.retrySignOut());
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(logout).toHaveBeenCalledTimes(SIGN_OUT_RETRY_DELAYS_MS.length + 1);

    await act(() => vi.advanceTimersByTimeAsync(SIGN_OUT_RETRY_DELAYS_MS.at(-1)! + 1));
    logout.mockReturnValue({ status: 204 } as never);
    await act(async () => {
      window.dispatchEvent(new Event("online"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(result.current.signOut).toBeNull();
  });

  it("network errors and a hung logout count as not confirmed", async () => {
    vi.useFakeTimers();
    signIn(alice);
    let calls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        calls++;
        if (calls === 1) return Promise.reject(new TypeError("Failed to fetch"));
        // Hangs until aborted (the timeout).
        return new Promise((_, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))));
      }),
    );
    const { result } = renderHook(() => useSession(vi.fn()));
    act(() => result.current.logout());
    await act(() => vi.advanceTimersByTimeAsync(SIGN_OUT_RETRY_DELAYS_MS[1]));
    expect(calls).toBe(2);
    expect(result.current.signOut).toBe("retrying");
    await act(() => vi.advanceTimersByTimeAsync(SESSION_REQUEST_TIMEOUT_MS));
    expect(result.current.signOut).toBe("retrying");
    expect(currentSession()).toBeNull();
  });

  it("never signs back in while a sign-out is pending: no refresh, nothing adopted, not even after a reload", async () => {
    signIn(alice);
    const fetch = stubFetch(on("POST", "/api/v1/auth/logout", busy), on("POST", "/api/v1/auth/refresh", { body: authResponse(alice, { token: "tok-r" }) }));
    const first = renderHook(() => useSession(vi.fn()));
    act(() => first.result.current.logout());
    act(() => otherTab().signsIn(authResponse(alice, { token: "tok-other" })));
    expect(currentSession()).toBeNull();
    await expect(refreshSession()).resolves.toBeNull();
    first.unmount();

    // A reload: memory is gone, the flag in this tab's sessionStorage is not.
    resetAuthForTests();
    const { result } = renderHook(() => useSession(vi.fn()));
    expect(result.current.checking).toBe(false);
    expect(result.current.signOut).toBe("signing-out");
    await waitFor(() => expect(callsTo(fetch, "/api/v1/auth/logout").length).toBeGreaterThan(1));
    expect(callsTo(fetch, "/api/v1/auth/refresh")).toHaveLength(0);
    expect(result.current.session).toBeNull();
    // After the reload only the cookie identifies the session (no token survived).
    const [, init] = callsTo(fetch, "/api/v1/auth/logout").at(-1)!;
    expect(new Headers(init?.headers).get("Authorization")).toBeNull();
  });

  it("a login replaces the unconfirmed sign-out", async () => {
    signIn(alice);
    stubFetch(on("POST", "/api/v1/auth/logout", busy));
    const { result } = renderHook(() => useSession(vi.fn()));
    act(() => result.current.logout());
    act(() => result.current.signIn(authResponse(alice, { token: "tok-new" })));
    expect(result.current.signOut).toBeNull();
    expect(result.current.session?.token).toBe("tok-new");
    expect(sessionStorage.getItem(SIGN_OUT_PENDING_KEY)).toBeNull();
  });

  it("another tab's sign-out hides this tab at once and ends it when confirmed", () => {
    signIn(alice);
    const fetch = stubFetch();
    const onSignOut = vi.fn();
    const { result } = renderHook(() => useSession(onSignOut));
    const other = new FakeBroadcastChannel(AUTH_CHANNEL);
    act(() => other.postMessage({ type: "signing-out" }));
    expect(result.current.session).toBeNull();
    expect(result.current.signOut).toBe("elsewhere");
    expect(onSignOut).toHaveBeenCalledOnce();
    // Only the tab that started it calls the server.
    expect(fetch).not.toHaveBeenCalled();
    act(() => other.postMessage({ type: "signed-out" }));
    expect(result.current.signOut).toBeNull();
    expect(result.current.notice).toBe(SIGNED_OUT_ELSEWHERE);
  });
});

describe("refresh retries inside the server's reuse grace", () => {
  it("retries a refresh lost on the network at once", async () => {
    let calls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        calls++;
        if (calls === 1) throw new TypeError("Failed to fetch");
        return new Response(JSON.stringify(authResponse(alice, { token: "tok-1" })), { status: 200 });
      }),
    );
    await expect(refreshSession()).resolves.toMatchObject({ token: "tok-1" });
    expect(calls).toBe(2);
  });

  it("retries a 5xx at once, but not a 429", async () => {
    const refresh = vi.fn().mockReturnValueOnce(busy).mockReturnValue({ body: authResponse(alice, { token: "tok-1" }) });
    stubFetch(on("POST", "/api/v1/auth/refresh", refresh));
    await expect(refreshSession()).resolves.toMatchObject({ token: "tok-1" });
    expect(refresh).toHaveBeenCalledTimes(2);

    const throttled = vi.fn(() => ({ status: 429, body: { error: { code: "rate_limited", message: "slow down" } } }));
    stubFetch(on("POST", "/api/v1/auth/refresh", throttled));
    await expect(refreshSession()).rejects.toMatchObject({ status: 429 });
    expect(throttled).toHaveBeenCalledOnce();
  });

  it("times a hung refresh out and retries it", async () => {
    vi.useFakeTimers();
    let calls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
        calls++;
        if (calls === 1) {
          return new Promise((_, reject) => init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))));
        }
        return Promise.resolve(new Response(JSON.stringify(authResponse(alice, { token: "tok-1" })), { status: 200 }));
      }),
    );
    const pending = refreshSession();
    await vi.advanceTimersByTimeAsync(SESSION_REQUEST_TIMEOUT_MS);
    await expect(pending).resolves.toMatchObject({ token: "tok-1" });
    expect(calls).toBe(2);
  });
});

describe("login", () => {
  it("sends JSON with its Content-Type", async () => {
    const fetch = stubFetch(on("POST", "/api/v1/auth/login", { body: authResponse(alice) }));
    await api.login({ email: "a@x", password: "password1" });
    const [, init] = fetch.mock.calls[0];
    expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
    expect(init?.credentials).toBe("same-origin");
  });
});
