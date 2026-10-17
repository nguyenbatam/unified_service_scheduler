import { useEffect, useRef, useState } from "react";
import { api, type AvailabilityQuery } from "./api";
import { dealershipToday, firstBookableDay, rescheduleExclusion } from "./booking";
import { formatDate, newIdempotencyKey } from "./time";
import type { RunOptions } from "./useAsyncAction";
import { Status, type Appointment, type Availability, type Dealership, type NextAvailability, type NextSlot, type Slot } from "./types";

type Run = <T>(fn: () => Promise<T>, opts?: RunOptions) => Promise<T | undefined>;

type NextQuery = AvailabilityQuery & {
  from?: string;
  days?: number;
  limit?: number;
  perDay?: number;
  prefer?: "recommended";
};

type GridParams = {
  dealershipId: string;
  vehicleId: string;
  serviceTypeIds: string[];
  date: string;
  isWaiter: boolean;
  excludeAppointmentId?: string;
};

type Options = {
  run: Run;
  dealerships: Dealership[];
  /** The appointment shown in the panel; it becomes the reschedule target only via startReschedule. */
  appointment: Appointment | null;
  /** Staff start on the dealership's today; customers on the first day they can book online. */
  isStaff?: boolean;
};

const isReady = (p: GridParams) => Boolean(p.dealershipId && p.vehicleId && p.serviceTypeIds.length > 0 && p.date);

/** Job lines of an appointment, in booked order (older rows may only carry the primary service). */
function linesOf(a: Appointment): string[] {
  return a.lines.length > 0 ? a.lines.map((l) => l.serviceTypeId) : [a.serviceTypeId];
}

const sameStart = (a: string, b: string) => Date.parse(a) === Date.parse(b);

/** A suggested start the fresh grid no longer offers. */
export const NEXT_SLOT_TAKEN = "That time was just taken — the suggestions were refreshed, please pick another time.";

/**
 * The suggestion list's shape: a few starts spread over several days (the grid already lists one
 * day in full). Staff get the earliest, to read out on the phone; customers each day's gap-free
 * starts first, so their pick packs the workshop.
 */
const STAFF_NEXT = { days: 14, limit: 6, perDay: 3 } as const;
const CUSTOMER_NEXT = { days: 14, limit: 4, perDay: 2, prefer: "recommended" } as const;

/** The picked slot if the fresh grid still offers it, else null (taken meanwhile). */
function stillOffered(picked: Slot | null, grid: Availability): Slot | null {
  if (!picked) return null;
  return grid.slots.find((s) => s.available && sameStart(s.start, picked.start)) ?? null;
}

/**
 * State of the booking form (dealership → vehicle → job lines → date → slots → confirm) and the
 * reschedule mode. Every change of an input drops the grid, the picked slot, and any response still
 * in flight for the old inputs, so Confirm can never book a slot of another day, car, or job.
 * While rescheduling, the car, job lines, and dealership are the appointment's (the UI locks them);
 * changing one anyway (a walk-in car picked) ends reschedule mode, so a move never silently turns
 * into a new booking.
 */
export function useBookingForm({ run, dealerships, appointment, isStaff = false }: Options) {
  const [dealershipId, setDealershipId] = useState("");
  const [vehicleId, setVehicleId] = useState("");
  /** Job lines in pick order; the first is the primary service. */
  const [serviceTypeIds, setServiceTypeIds] = useState<string[]>([]);
  const [date, setDate] = useState("");
  const [isWaiter, setIsWaiter] = useState(false);
  const [availability, setAvailability] = useState<Availability | null>(null);
  const [nextSlots, setNextSlots] = useState<NextAvailability | null>(null);
  const [selectedSlot, setSelectedSlot] = useState<Slot | null>(null);
  // The Idempotency-Key of the booking request last sent or about to be: kept while the request
  // (dealership, car, job lines, start, waiter flag) is unchanged, so a retry after a timeout or a
  // network error (the booking may have been saved) replays instead of booking twice.
  const bookingKeyRef = useRef<{ sig: string; key: string } | null>(null);
  const [slotsLoading, setSlotsLoading] = useState(false);
  // Set only by an explicit "Reschedule": viewing an appointment never changes the form.
  const [rescheduleTargetId, setRescheduleTargetId] = useState<string | null>(null);

  // Responses are applied only while they are the latest request for the current inputs.
  const availSeq = useRef(0);
  const nextSeq = useRef(0);
  const shown = useRef(false);
  shown.current = availability !== null;
  // The query behind the first-available list on screen, re-run by a silent refresh.
  const nextQuery = useRef<NextQuery | null>(null);
  const nextShown = useRef(false);
  nextShown.current = nextSlots !== null;

  const dealership = dealerships.find((d) => d.id === dealershipId);
  const rescheduleTarget = appointment && appointment.id === rescheduleTargetId ? appointment : null;
  // Moving an appointment: availability ignores its own hold, as the server's reschedule does, so
  // it can slide into an overlapping slot (e.g. 10:00–12:00 → 10:30).
  const excludeAppointmentId = rescheduleExclusion(rescheduleTarget, { dealershipId, vehicleId, serviceTypeIds });
  const current: GridParams = { dealershipId, vehicleId, serviceTypeIds, date, isWaiter, excludeAppointmentId };
  const ready = isReady(current);
  // The inputs as last rendered: a refresh started by a caller holding an older render (a write
  // that awaited the server) must read the grid of what the form shows now, not of then.
  const inputs = useRef(current);
  inputs.current = current;
  // By the clock in the dealership's zone: a tab left open past midnight starts on the real today.
  const startDay = (d: Dealership) => (isStaff ? dealershipToday(d) : firstBookableDay(d));

  function clearSlotSelection() {
    availSeq.current++;
    nextSeq.current++;
    setAvailability(null);
    setNextSlots(null);
    setSelectedSlot(null);
    setSlotsLoading(false);
  }

  /**
   * The Idempotency-Key for `request`: the same key while the request is the same (a re-click of
   * the same slot, a retry after 503 or a network error), a new one as soon as anything in it
   * changed.
   */
  function bookingKey(request: object): string {
    const sig = JSON.stringify(request);
    if (bookingKeyRef.current?.sig !== sig) bookingKeyRef.current = { sig, key: newIdempotencyKey() };
    return bookingKeyRef.current.key;
  }

  /** The last booking request has a definite outcome (booked, or refused): the next one gets a new key. */
  function settleBookingKey() {
    bookingKeyRef.current = null;
  }

  async function fetchGrid(p: GridParams, opts?: { silent?: boolean; resetSelection?: boolean }) {
    const seq = ++availSeq.current;
    if (opts?.resetSelection) setSelectedSlot(null);
    if (!opts?.silent) setSlotsLoading(true);
    try {
      const result = await api.availability(p);
      if (seq !== availSeq.current) return null;
      setAvailability(result);
      // A live refresh may show the picked start taken by someone else: drop it so Confirm cannot
      // send a booking that is sure to be refused.
      setSelectedSlot((picked) => stillOffered(picked, result));
      return result;
    } catch (err) {
      if (opts?.silent || seq !== availSeq.current) return null;
      throw err;
    } finally {
      if (seq === availSeq.current) setSlotsLoading(false);
    }
  }

  /** Runs a first-available search; the response applies only while it is the latest one. */
  async function fetchNext(q: NextQuery, opts?: { silent?: boolean }) {
    const seq = ++nextSeq.current;
    nextQuery.current = q;
    try {
      const result = await api.nextAvailability(q);
      return seq === nextSeq.current ? result : null;
    } catch (err) {
      if (opts?.silent || seq !== nextSeq.current) return null;
      throw err;
    }
  }

  /**
   * Re-reads the grid for the current inputs. `silent` (live events, refresh after an action) never
   * reports an error and does nothing until the user has loaded a grid. `next` also re-reads the
   * first-available list on screen (a change that may move any day: hours, roster, a service's
   * duration, a booking on another day); `grid: false` re-reads only that list. The picked slot
   * stays while the fresh grid still offers it.
   */
  async function refreshAvailability(opts?: { silent?: boolean; resetSelection?: boolean; next?: boolean; grid?: boolean }) {
    const p = inputs.current;
    if (!isReady(p)) return;
    const list = opts?.next ? refreshNext({ silent: opts.silent }) : null;
    const grid = opts?.grid === false || (opts?.silent && !shown.current) ? null : fetchGrid(p, opts);
    await Promise.all([grid, list]);
  }

  /** Re-runs the first-available search on screen (nothing when none is shown). */
  async function refreshNext(opts?: { silent?: boolean }) {
    const q = nextQuery.current;
    if (!nextShown.current || !q) return;
    const fresh = await fetchNext(q, opts);
    if (fresh) setNextSlots(fresh);
  }

  // The reschedule target stopped being movable (checked in elsewhere, cancelled): the grid was
  // computed without its hold, so re-read it.
  const lastExclusion = useRef(excludeAppointmentId);
  useEffect(() => {
    if (lastExclusion.current === excludeAppointmentId) return;
    lastExclusion.current = excludeAppointmentId;
    void refreshAvailability({ silent: true, resetSelection: true });
  });

  // The appointment being moved stopped being movable (checked in, cancelled elsewhere): leave
  // reschedule mode instead of offering times it can no longer take.
  useEffect(() => {
    if (rescheduleTargetId && appointment?.id === rescheduleTargetId && appointment.status !== Status.Confirmed) {
      setRescheduleTargetId(null);
    }
  }, [rescheduleTargetId, appointment?.id, appointment?.status]);

  async function loadAvailability() {
    if (!ready) return;
    await run(async () => {
      nextSeq.current++;
      setNextSlots(null);
      await fetchGrid(current, { resetSelection: true });
    });
  }

  async function findFirstAvailable() {
    if (!ready || !dealership) return;
    await run(async () => {
      const result = await fetchNext({
        dealershipId,
        vehicleId,
        serviceTypeIds,
        isWaiter,
        excludeAppointmentId,
        from: date >= dealership.today ? date : undefined,
        ...(isStaff ? STAFF_NEXT : CUSTOMER_NEXT),
      });
      if (result) setNextSlots(result);
    });
  }

  /**
   * Jumps the grid to the picked day and selects that start, so Confirm books it as usual — only
   * when the fresh grid still offers it. Taken meanwhile: nothing is selected, the list is re-read,
   * and the user is told (the grid of that day shows why).
   */
  async function pickNextSlot(slot: NextSlot) {
    await run(async () => {
      setDate(slot.date);
      setSelectedSlot(null);
      const result = await fetchGrid({ ...current, date: slot.date });
      if (!result) return;
      const match = result.slots.find((s) => s.available && sameStart(s.start, slot.start));
      if (match) {
        setSelectedSlot(match);
        return;
      }
      void refreshNext({ silent: true });
      throw new Error(NEXT_SLOT_TAKEN);
    });
  }

  function selectSlot(slot: Slot) {
    setSelectedSlot(slot);
  }

  /** Back to a blank form (sign-out, before seeding the next user): nothing of the last user stays. */
  function reset() {
    clearSlotSelection();
    settleBookingKey();
    setDealershipId("");
    setVehicleId("");
    setServiceTypeIds([]);
    setDate("");
    setIsWaiter(false);
    setRescheduleTargetId(null);
    lastExclusion.current = undefined;
  }

  /**
   * First sign-in: staff start at their home dealership on its today; everyone else at the first
   * one, on the first day the lead time leaves bookable (today may be all "too soon").
   */
  function seed(home: Dealership | undefined, firstServiceId: string | undefined) {
    if (home) {
      setDealershipId(home.id);
      setDate(startDay(home));
    }
    if (firstServiceId) setServiceTypeIds([firstServiceId]);
  }

  function changeDealership(id: string) {
    const next = dealerships.find((d) => d.id === id);
    setRescheduleTargetId(null);
    setDealershipId(id);
    if (next) setDate(startDay(next));
    clearSlotSelection();
  }

  /**
   * After an input changed: drop the old grid at once, and when the user had one on screen, load
   * the grid for the new inputs without another click (as for a new day).
   */
  function reloadFor(next: GridParams) {
    const reload = shown.current && isReady(next);
    clearSlotSelection();
    if (reload) void run(() => fetchGrid(next));
  }

  function changeVehicle(id: string) {
    setRescheduleTargetId(null);
    setVehicleId(id);
    // Another car ends reschedule mode, so the grid no longer ignores the appointment's hold.
    reloadFor({ ...current, vehicleId: id, excludeAppointmentId: undefined });
  }

  /** Seeds the vehicle from the first page of the list, never replacing a choice already made. */
  function defaultVehicle(id: string) {
    setVehicleId((prev) => prev || id);
  }

  /** A new day: the old grid goes at once, and a grid the user had loaded is re-read for that day. */
  function changeDate(next: string) {
    setDate(next);
    reloadFor({ ...current, date: next });
  }

  function changeWaiter(next: boolean) {
    setIsWaiter(next);
    reloadFor({ ...current, isWaiter: next });
  }

  function changeServices(ids: string[]) {
    setRescheduleTargetId(null);
    setServiceTypeIds(ids);
    reloadFor({ ...current, serviceTypeIds: ids, excludeAppointmentId: undefined });
  }

  /**
   * A customer's first look: the earliest free starts over the coming days, and the grid of the
   * first one's day, so the form never opens on a day that is all "too soon" or full.
   */
  async function openFirstAvailable() {
    if (!ready || !dealership) return;
    await run(async () => {
      const result = await fetchNext({
        dealershipId,
        vehicleId,
        serviceTypeIds,
        isWaiter,
        from: date >= dealership.today ? date : undefined,
        ...(isStaff ? STAFF_NEXT : CUSTOMER_NEXT),
      });
      if (!result) return;
      setNextSlots(result);
      const first = result.slots[0];
      if (!first) return;
      setDate(first.date);
      await fetchGrid({ ...current, date: first.date });
    });
  }

  /** Points the form at `a` (its car, job lines, waiter flag, and day) and loads where it can move. */
  async function startReschedule(a: Appointment, timeZone: string) {
    const day = formatDate(a.startAt, timeZone);
    const lines = linesOf(a);
    setRescheduleTargetId(a.id);
    setDealershipId(a.dealershipId);
    setVehicleId(a.vehicleId);
    setServiceTypeIds(lines);
    setIsWaiter(a.isWaiter);
    setDate(day);
    clearSlotSelection();
    lastExclusion.current = a.id;
    await run(() =>
      fetchGrid({
        dealershipId: a.dealershipId,
        vehicleId: a.vehicleId,
        serviceTypeIds: lines,
        date: day,
        isWaiter: a.isWaiter,
        excludeAppointmentId: a.id,
      }),
    );
  }

  function stopReschedule() {
    if (rescheduleTargetId === null) return;
    setRescheduleTargetId(null);
    clearSlotSelection();
  }

  return {
    dealershipId,
    dealership,
    vehicleId,
    serviceTypeIds,
    date,
    isWaiter,
    availability,
    nextSlots,
    selectedSlot,
    bookingKey,
    settleBookingKey,
    slotsLoading,
    ready,
    rescheduleTarget,
    excludeAppointmentId,
    reset,
    seed,
    changeDealership,
    changeVehicle,
    defaultVehicle,
    changeDate,
    changeWaiter,
    changeServices,
    clearSlotSelection,
    refreshAvailability,
    loadAvailability,
    findFirstAvailable,
    openFirstAvailable,
    pickNextSlot,
    selectSlot,
    startReschedule,
    stopReschedule,
    /** After a booking: leave reschedule mode without dropping the grid (it is re-read silently). */
    finishReschedule: () => setRescheduleTargetId(null),
  };
}

export type BookingForm = ReturnType<typeof useBookingForm>;
