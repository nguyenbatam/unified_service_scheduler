import { afterEach, describe, expect, it, vi } from "vitest";
import { FETCH_ALL_MAX, FETCH_ALL_PAGE, NetworkError, api, appointmentPageParams, fetchAll, fetchPage, isApiError } from "./api";

describe("appointment pages", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("sends only the set query parameters", () => {
    expect(appointmentPageParams({}).toString()).toBe("");
    expect(appointmentPageParams({ limit: 20, cursor: "abc_-1" }).toString()).toBe(
      "limit=20&cursor=abc_-1",
    );
    expect(appointmentPageParams({ customerId: "c1", when: "upcoming", limit: 20 }).toString()).toBe(
      "customerId=c1&when=upcoming&limit=20",
    );
  });

  it("reads the next cursor from X-Next-Cursor", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response("[]", { headers: { "X-Next-Cursor": "next-1" } }))
      .mockResolvedValueOnce(new Response("[]"));
    vi.stubGlobal("fetch", fetchMock);
    expect(await api.appointmentPage({ limit: 2 })).toEqual({ items: [], nextCursor: "next-1" });
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/appointments?limit=2");
    expect(await api.appointmentPage({ cursor: "next-1" })).toEqual({ items: [], nextCursor: null });
  });
});

describe("list pages", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("fetchPage adds the page to the filters and reads the next token", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    const fetchMock = vi.fn().mockResolvedValue(new Response('[{"id":"a"}]', { headers: { "X-Next-Cursor": "t2" } }));
    vi.stubGlobal("fetch", fetchMock);
    const page = await fetchPage<{ id: string }>("/api/v1/customers", new URLSearchParams({ q: "ann" }), { limit: 20, cursor: "t1" });
    expect(page).toEqual({ items: [{ id: "a" }], nextCursor: "t2" });
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/customers?q=ann&limit=20&cursor=t1");
  });

  it("fetchAll follows X-Next-Cursor to the last page, keeping the filters, each row once", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response('[{"id":"a"},{"id":"b"}]', { headers: { "X-Next-Cursor": "p2" } }))
      // "b" was renamed between the pages and comes again after the cursor: kept once.
      .mockResolvedValueOnce(new Response('[{"id":"b"},{"id":"c"}]'));
    vi.stubGlobal("fetch", fetchMock);
    const all = await fetchAll<{ id: string }>("/api/v1/admin/technicians", new URLSearchParams({ dealershipId: "d1" }));
    expect(all.items.map((t) => t.id)).toEqual(["a", "b", "c"]);
    expect(all.truncated).toBe(false);
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      `/api/v1/admin/technicians?dealershipId=d1&limit=${FETCH_ALL_PAGE}`,
      `/api/v1/admin/technicians?dealershipId=d1&limit=${FETCH_ALL_PAGE}&cursor=p2`,
    ]);
  });

  it("fetchAll stops at its cap however many pages the server offers", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    let n = 0;
    const fetchMock = vi.fn(async () => {
      const page = Array.from({ length: FETCH_ALL_PAGE }, () => ({ id: String(n++) }));
      return new Response(JSON.stringify(page), { headers: { "X-Next-Cursor": "more" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const all = await api.dealerships();
    expect(all.items).toHaveLength(FETCH_ALL_MAX);
    expect(all.truncated).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(Math.ceil(FETCH_ALL_MAX / FETCH_ALL_PAGE));
  });

  it("fetchAll is not truncated when the last page ends exactly at its cap", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    let n = 0;
    const fetchMock = vi.fn(async () => {
      const page = Array.from({ length: FETCH_ALL_PAGE }, () => ({ id: String(n++) }));
      return new Response(JSON.stringify(page), n < FETCH_ALL_MAX ? { headers: { "X-Next-Cursor": "more" } } : {});
    });
    vi.stubGlobal("fetch", fetchMock);
    const all = await fetchAll<{ id: string }>("/api/v1/dealerships");
    expect(all.items).toHaveLength(FETCH_ALL_MAX);
    expect(all.truncated).toBe(false);
  });
});

describe("api errors", () => {
  afterEach(() => vi.unstubAllGlobals());

  const failWith = async (res: Response) => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(res));
    return api.appointmentPage().then(
      () => {
        throw new Error("expected a rejection");
      },
      (err: unknown) => err,
    );
  };

  it("reports a non-JSON error page by status", async () => {
    const err = await failWith(new Response("<html>bad gateway</html>", { status: 502 }));
    expect(isApiError(err)).toBe(true);
    expect(err).toMatchObject({ status: 502, code: "http_error", message: "HTTP 502" });
  });

  it("keeps a positive numeric Retry-After only", async () => {
    const limited = await failWith(
      new Response('{"error":{"code":"rate_limited","message":"slow down"}}', {
        status: 429,
        headers: { "Retry-After": "30" },
      }),
    );
    expect(limited).toMatchObject({ status: 429, code: "rate_limited", retryAfter: 30 });
    for (const value of ["0", "Wed, 21 Oct 2026 07:28:00 GMT", "soon"]) {
      const err = await failWith(new Response("{}", { status: 429, headers: { "Retry-After": value } }));
      expect(err).not.toHaveProperty("retryAfter");
    }
  });

  it("wraps a request that got no answer in a NetworkError, but not an abort", async () => {
    vi.stubGlobal("localStorage", { getItem: () => null, removeItem: () => {} });
    vi.stubGlobal("fetch", vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch")));
    const err = await api.appointmentPage().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(NetworkError);
    expect(isApiError(err)).toBe(false);
    expect((err as Error).cause).toBeInstanceOf(TypeError);

    const abort = new DOMException("The user aborted a request.", "AbortError");
    vi.stubGlobal("fetch", vi.fn().mockRejectedValueOnce(abort));
    expect(await api.appointmentPage().catch((e: unknown) => e)).toBe(abort);
  });
});
