import { useEffect, useRef, useState } from "react";
import { api, appendPage } from "./api";
import type { Vehicle } from "./types";
import { errorText } from "./errors";
import {
  CUSTOMER_VEHICLE_LIMIT,
  STAFF_VEHICLE_LIMIT,
  VEHICLE_SEARCH_DEBOUNCE_MS,
  upsertVehicle,
  vehicleOptions,
} from "./vehicles";

export type VehicleList = {
  query: string;
  setQuery: (q: string) => void;
  /** The page plus the selected vehicle when the page does not hold it. */
  options: Vehicle[];
  loaded: boolean;
  /** More vehicles match than are listed: `more` appends the next page. */
  hasMore: boolean;
  loadingMore: boolean;
  more: () => void;
  /** List a vehicle just created. */
  add: (v: Vehicle) => void;
};

type Options = {
  /** The session's key (auth.ts): undefined until signed in and loaded; a new one starts over with an empty search. */
  sessionKey: string | undefined;
  isStaff: boolean;
  selectedId: string;
  /** The first unfiltered page of a session (seeds the selection). */
  onFirstPage: (items: Vehicle[]) => void;
  onError: (message: string) => void;
};

/**
 * Vehicles for the booking form, one page at a time (GET /vehicles?q=&limit=&cursor=, "Show more"
 * appends the next): staff search, debounced; customers get their own garage. The selected vehicle is fetched by id when the page does
 * not hold it (a focused appointment's car), so the select can always show it.
 */
export function useVehicleList(opts: Options): VehicleList {
  const { sessionKey: token, isStaff, selectedId } = opts;
  const limit = isStaff ? STAFF_VEHICLE_LIMIT : CUSTOMER_VEHICLE_LIMIT;
  const [owner, setOwner] = useState(token);
  const [query, setQuery] = useState("");
  const [page, setPage] = useState<Vehicle[] | null>(null);
  // The next page's token for the listed search (null: last page); reset with every new search.
  const [next, setNext] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  // Last selected vehicle seen (in a page, by id, or just created): kept when a search drops it.
  const [selected, setSelected] = useState<Vehicle | null>(null);
  if (owner !== token) {
    // Another session: forget the previous user's search and vehicles (reset while rendering).
    setOwner(token);
    setQuery("");
    setPage(null);
    setNext(null);
    setSelected(null);
  }
  const latest = useRef(opts);
  latest.current = opts;
  const seededFor = useRef<string | undefined>(undefined);

  const q = query.trim();
  // Bumped per first-page load: a "Show more" answer for an older search is dropped.
  const generation = useRef(0);
  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    ++generation.current;
    setNext(null);
    const timer = setTimeout(
      () => {
        api
          .vehicles({ q, limit })
          .then(({ items, nextCursor }) => {
            if (cancelled) return;
            setPage(items);
            setNext(nextCursor);
            if (!q && seededFor.current !== token) {
              seededFor.current = token;
              latest.current.onFirstPage(items);
            }
          })
          .catch((err: unknown) => {
            if (!cancelled) latest.current.onError(errorText(err));
          });
      },
      q ? VEHICLE_SEARCH_DEBOUNCE_MS : 0,
    );
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [token, q, limit]);

  const inPage = page?.find((v) => v.id === selectedId) ?? null;
  useEffect(() => {
    if (inPage) setSelected(inPage);
  }, [inPage]);

  const missing = Boolean(token && page && selectedId && !inPage && selected?.id !== selectedId);
  useEffect(() => {
    if (!missing) return;
    let cancelled = false;
    api
      .vehicle(selectedId)
      .then((v) => {
        if (!cancelled) setSelected(v);
      })
      .catch(() => undefined); // gone or not ours: the select just cannot label it
    return () => {
      cancelled = true;
    };
  }, [missing, selectedId]);

  function more() {
    if (!next || loadingMore) return;
    const gen = generation.current;
    setLoadingMore(true);
    api
      .vehicles({ q, limit, cursor: next })
      .then(({ items, nextCursor }) => {
        if (gen !== generation.current) return;
        setPage((prev) => appendPage(prev ?? [], items));
        setNext(nextCursor);
      })
      .catch((err: unknown) => {
        if (gen === generation.current) latest.current.onError(errorText(err));
      })
      .finally(() => setLoadingMore(false));
  }

  return {
    query,
    setQuery,
    options: vehicleOptions(page ?? [], selected, selectedId),
    loaded: page !== null,
    hasMore: next !== null,
    loadingMore,
    more,
    add: (v) => {
      setSelected(v);
      setPage((prev) => (prev && !q ? upsertVehicle(prev, v) : prev));
    },
  };
}
