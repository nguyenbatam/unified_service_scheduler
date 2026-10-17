import { useCallback, useEffect, useRef, useState } from "react";
import { appendPage } from "./api";
import { errorText } from "./errors";
import type { Page } from "./types";

export type PagedList<T> = {
  /** The pages read so far, in order; null until the first page of this listing arrives. */
  items: T[] | null;
  /** A request is in flight (first page, reload, or "Show more"). */
  loading: boolean;
  error: string;
  /** The server has more rows after the last page read. */
  hasMore: boolean;
  /** Appends the next page (no-op without one, while a request is in flight, or while reloading). */
  more: () => Promise<void>;
};

type Options<T> = {
  /** Rows per page: the first page and each "Show more". */
  pageSize: number;
  /**
   * The API's maximum `limit` for this listing (default pageSize): a reload re-reads as many rows
   * as are shown, in requests of at most this many.
   */
  maxPageSize?: number;
  /** false: no request, an empty list (e.g. a search shorter than 2 characters). Default true. */
  enabled?: boolean;
  /** Debounce before the first page (a search box); default 0. */
  delayMs?: number;
  /**
   * Changing it re-reads the listing (live refresh), keeping the rows shown until the answer
   * arrives: as many rows as are shown (at least a page), so a list expanded with "Show more"
   * stays expanded, and its next cursor continues after the fresh rows.
   */
  reloadKey?: unknown;
  /** A row's identity, so a row seen on an earlier page is not added twice; default `item.id`. */
  idOf?: (item: T) => string;
};

const defaultID = (item: unknown) => String((item as { id?: unknown }).id);

/** The next page's token and the (re)load generation whose rows it continues. */
type Cursor = { token: string; gen: number };

/**
 * readRows reads a listing's first `want` rows from the start, in requests of at most `max`,
 * following the cursor (a row already read is not added twice). It stops at the last page, once
 * `want` rows came, when the server answers a shorter page than asked (its page is what counts),
 * or once `live()` is false (a newer load took over). Returns the rows and the cursor after them.
 */
export async function readRows<T>(
  load: (cursor: string | null, limit: number) => Promise<Page<T>>,
  want: number,
  max: number,
  idOf: (item: T) => string,
  live: () => boolean = () => true,
): Promise<Page<T>> {
  let items: T[] = [];
  let cursor: string | null = null;
  let received = 0;
  let full = true;
  do {
    const limit = Math.min(want - received, max);
    const page: Page<T> = await load(cursor, limit);
    items = appendPage(items, page.items, idOf);
    received += page.items.length;
    full = page.items.length >= limit;
    cursor = page.nextCursor;
  } while (cursor !== null && full && received < want && live());
  return { items, nextCursor: cursor };
}

/**
 * usePagedList reads a paged list endpoint one page at a time ("Show more"). `key` names the
 * listing and its filters: a new key starts over from the first page (the old rows go at once),
 * and any answer meant for an older key or an older reload is dropped, so a slow page never lands
 * in a newer list. `load` reads one page of at most `limit` rows after `cursor` (null: the first).
 */
export function usePagedList<T>(
  load: (cursor: string | null, limit: number) => Promise<Page<T>>,
  key: string,
  { pageSize, maxPageSize = pageSize, enabled = true, delayMs = 0, reloadKey, idOf = defaultID }: Options<T>,
): PagedList<T> {
  const [items, setItems] = useState<T[] | null>(null);
  const [cursor, setCursor] = useState<Cursor | null>(null);
  // Requests of the current generation in flight (a reload may overlap a "Show more"); loading
  // while any is. A new generation starts at 0: an abandoned request never keeps it loading.
  const [inFlight, setInFlight] = useState(0);
  // A same-key reload started and has not answered yet (its request may still be waiting for its
  // timer): loading, and "Show more" waits for the fresh cursor.
  const [reloading, setReloading] = useState(false);
  const [error, setError] = useState("");
  // Bumped per (re)load: answers of an older generation are dropped, and so is a cursor it made.
  const generation = useRef(0);
  const latest = useRef({ load, idOf, cursor, items, pageSize, maxPageSize, busy: false });
  latest.current = { ...latest.current, load, idOf, cursor, items, pageSize, maxPageSize };
  // The listing whose rows are shown (null: none, e.g. while disabled).
  const shownKey = useRef<string | null>(null);

  const track = useCallback(<R,>(gen: number, request: Promise<R>, apply: (r: R) => void) => {
    setInFlight((n) => n + 1);
    return request
      .then((r) => {
        if (gen === generation.current) apply(r);
      })
      .catch((err: unknown) => {
        if (gen === generation.current) setError(errorText(err));
      })
      .finally(() => {
        if (gen === generation.current) setInFlight((n) => Math.max(0, n - 1));
      });
  }, []);

  useEffect(() => {
    const gen = ++generation.current;
    const reload = shownKey.current === key && enabled;
    if (!reload) {
      // Another listing (or none): never show the previous one's rows under it.
      shownKey.current = enabled ? key : null;
      setItems(enabled ? null : []);
      setCursor(null);
    }
    setError("");
    setInFlight(0);
    setReloading(reload);
    if (!enabled) return;
    // A reload re-reads as many rows as are shown, so an expanded list stays expanded.
    const { pageSize: size, maxPageSize: max } = latest.current;
    const want = reload ? Math.max(size, latest.current.items?.length ?? 0) : size;
    const timer = setTimeout(() => {
      const { load: read, idOf: id } = latest.current;
      void track(gen, readRows(read, Math.max(want, 1), Math.max(max, 1), id, () => gen === generation.current), (page) => {
        setItems(page.items);
        setCursor(page.nextCursor === null ? null : { token: page.nextCursor, gen });
      }).finally(() => {
        if (gen !== generation.current) return;
        setReloading(false);
        // A failed reload leaves the rows shown, and the cursor after them still continues them.
        setCursor((c) => (c === null ? null : { ...c, gen }));
      });
    }, delayMs);
    return () => clearTimeout(timer);
  }, [key, enabled, delayMs, reloadKey, track]);

  const more = useCallback(async () => {
    const { cursor: next, busy, pageSize: size } = latest.current;
    const gen = generation.current;
    // A cursor of an older generation continues rows a reload is replacing: wait for its own.
    if (!next || next.gen !== gen || busy) return;
    latest.current.busy = true;
    try {
      await track(gen, latest.current.load(next.token, size), (page) => {
        setItems((prev) => appendPage(prev ?? [], page.items, latest.current.idOf));
        setCursor(page.nextCursor === null ? null : { token: page.nextCursor, gen });
      });
    } finally {
      latest.current.busy = false;
    }
  }, [track]);

  return { items, loading: inFlight > 0 || reloading, error, hasMore: cursor !== null, more };
}
