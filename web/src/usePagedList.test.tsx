import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { appendPage } from "./api";
import { deferred } from "./testFixtures";
import type { Page } from "./types";
import { readRows, usePagedList } from "./usePagedList";

type Row = { id: string };
const rows = (...ids: string[]): Row[] => ids.map((id) => ({ id }));
const ids = (items: Row[] | null | undefined) => items?.map((r) => r.id);

/** A listing served by offset tokens ("@n"), recording each request as `cursor/limit`. */
function server(...initial: string[]) {
  let list = initial;
  const asked: string[] = [];
  const load = vi.fn(async (cursor: string | null, limit: number): Promise<Page<Row>> => {
    asked.push(`${cursor ?? "-"}/${limit}`);
    const from = cursor ? Number(cursor.slice(1)) : 0;
    const to = from + limit;
    return { items: rows(...list.slice(from, to)), nextCursor: to < list.length ? `@${to}` : null };
  });
  return { load, asked, set: (...next: string[]) => (list = next) };
}

describe("appendPage", () => {
  it("adds only rows not shown yet", () => {
    expect(ids(appendPage(rows("a", "b"), rows("b", "c")))).toEqual(["a", "b", "c"]);
  });
});

describe("readRows", () => {
  it("reads the first `want` rows in requests of at most `max`, each row once", async () => {
    const s = server("a", "b", "c", "d", "e", "f", "g");
    const page = await readRows(s.load, 5, 2, (r) => r.id);
    expect(ids(page.items)).toEqual(["a", "b", "c", "d", "e"]);
    expect(page.nextCursor).toBe("@5");
    expect(s.asked).toEqual(["-/2", "@2/2", "@4/1"]);
  });
});

describe("usePagedList", () => {
  it("appends the next page on more() until the last page", async () => {
    const s = server("a", "b", "c");
    const { result } = renderHook(() => usePagedList(s.load, "list", { pageSize: 2 }));
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
    expect(result.current.hasMore).toBe(true);
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c"]);
    expect(result.current.hasMore).toBe(false);
    expect(s.asked).toEqual(["-/2", "@2/2"]);
  });

  it("drops an older listing's late answer and starts the new one empty", async () => {
    const slow = deferred<Page<Row>>();
    const load = vi.fn((cursor: string | null, key?: string) => {
      void cursor;
      return key === "old" ? slow.promise : Promise.resolve({ items: rows("new"), nextCursor: null });
    });
    const { result, rerender } = renderHook(({ k }) => usePagedList<Row>((c) => load(c, k), k, { pageSize: 2 }), {
      initialProps: { k: "old" },
    });
    rerender({ k: "new" });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["new"]));
    await act(async () => slow.resolve({ items: rows("stale"), nextCursor: "x" }));
    expect(ids(result.current.items)).toEqual(["new"]);
    expect(result.current.hasMore).toBe(false);
  });

  it("is idle and empty while disabled, even with a request of the old query in flight", async () => {
    const slow = deferred<Page<Row>>();
    const { result, rerender } = renderHook(({ on }) => usePagedList(() => slow.promise, "q", { pageSize: 2, enabled: on }), {
      initialProps: { on: true },
    });
    await waitFor(() => expect(result.current.loading).toBe(true));
    rerender({ on: false });
    expect(result.current.loading).toBe(false);
    expect(result.current.items).toEqual([]);
    await act(async () => slow.resolve({ items: rows("late"), nextCursor: null }));
    expect(result.current.items).toEqual([]);
    expect(result.current.loading).toBe(false);
  });

  it("keeps the rows shown while a reload reads the first page again", async () => {
    let version = 1;
    const reload = deferred<Page<Row>>();
    const load = vi.fn(() => (version === 1 ? Promise.resolve({ items: rows("a"), nextCursor: null }) : reload.promise));
    const { result, rerender } = renderHook(({ r }) => usePagedList(load, "list", { pageSize: 2, reloadKey: r }), {
      initialProps: { r: 1 },
    });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a"]));
    version = 2;
    rerender({ r: 2 });
    expect(ids(result.current.items)).toEqual(["a"]);
    await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
    expect(ids(result.current.items)).toEqual(["a"]);
    await act(async () => reload.resolve({ items: rows("a", "b"), nextCursor: null }));
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
  });

  it("a reload of a list expanded with more() stays expanded, fresh, with the cursor after it", async () => {
    const s = server("a", "b", "c", "d", "e", "f");
    const { result, rerender } = renderHook(({ r }) => usePagedList(s.load, "list", { pageSize: 2, maxPageSize: 10, reloadKey: r }), {
      initialProps: { r: 1 },
    });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d"]);
    // A live change: "b" leaves the listing and "x" joins it.
    s.set("a", "c", "d", "x", "e", "f");
    rerender({ r: 2 });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "c", "d", "x"]));
    expect(s.asked.at(-1)).toBe("-/4");
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "c", "d", "x", "e", "f"]);
    expect(s.asked.at(-1)).toBe("@4/2");
    expect(result.current.hasMore).toBe(false);
  });

  it("a reload of a list longer than the API maximum pages up to the length shown", async () => {
    const s = server("a", "b", "c", "d", "e", "f", "g");
    const { result, rerender } = renderHook(({ r }) => usePagedList(s.load, "list", { pageSize: 2, maxPageSize: 2, reloadKey: r }), {
      initialProps: { r: 1 },
    });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
    await act(() => result.current.more());
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d", "e", "f"]);
    s.asked.length = 0;
    rerender({ r: 2 });
    await waitFor(() => expect(s.asked).toEqual(["-/2", "@2/2", "@4/2"]));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d", "e", "f"]);
    expect(result.current.hasMore).toBe(true);
  });

  it("Show more waits for a reload: never the old cursor under the new rows", async () => {
    const s = server("a", "b", "c", "d", "e", "f", "g", "h");
    const { result, rerender } = renderHook(({ r }) => usePagedList(s.load, "list", { pageSize: 2, maxPageSize: 10, reloadKey: r }), {
      initialProps: { r: 1 },
    });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d"]);
    // The reload is queued (its timer has not fired): loading, and more() does nothing.
    rerender({ r: 2 });
    expect(result.current.loading).toBe(true);
    const before = s.asked.length;
    await act(() => result.current.more());
    expect(s.asked.slice(before).filter((q) => q.startsWith("@4"))).toEqual([]);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d"]);
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c", "d", "e", "f"]);
    expect(s.asked.slice(before)).toEqual(["-/4", "@4/2"]);
  });

  it("a failed reload keeps the rows shown and their cursor usable", async () => {
    const s = server("a", "b", "c");
    let fail = false;
    const load = vi.fn((cursor: string | null, limit: number) => (fail ? Promise.reject(new Error("down")) : s.load(cursor, limit)));
    const { result, rerender } = renderHook(({ r }) => usePagedList(load, "list", { pageSize: 2, reloadKey: r }), {
      initialProps: { r: 1 },
    });
    await waitFor(() => expect(ids(result.current.items)).toEqual(["a", "b"]));
    fail = true;
    rerender({ r: 2 });
    await waitFor(() => expect(result.current.error).toBe("down"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(ids(result.current.items)).toEqual(["a", "b"]);
    fail = false;
    await act(() => result.current.more());
    expect(ids(result.current.items)).toEqual(["a", "b", "c"]);
  });
});
