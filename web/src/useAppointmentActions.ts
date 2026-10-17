import { type Dispatch, type SetStateAction, useRef } from "react";
import type { ActionOptions } from "./AppointmentPanel";
import { NetworkError, api, isApiError, type AppointmentAction } from "./api";
import { actionErrorText, bookingErrorText, isSlotConflict } from "./errors";
import { adoptFetched } from "./live";
import { formatClock, formatDate, formatDayLabel, zoneAbbr } from "./time";
import type { BookingForm } from "./useBookingForm";
import type { Appointment, Slot } from "./types";
import type { RunOptions } from "./useAsyncAction";

type Run = <T>(fn: () => Promise<T>, opts?: RunOptions) => Promise<T | undefined>;

type Options = {
  /** The appointment in the panel (the target of every action). */
  appointment: Appointment | null;
  setAppointment: Dispatch<SetStateAction<Appointment | null>>;
  form: BookingForm;
  run: Run;
  toast: (text: string) => void;
  refreshSchedule: (opts?: { silent?: boolean }) => Promise<void>;
  /** Reloads the appointment lists. */
  refreshLists: () => void;
  timezoneOf: (dealershipId: string) => string;
  /** Scrolls the appointment panel into view (after a booking or a move). */
  revealPanel: () => void;
};

/** "Mon, 19 Oct 2026 10:00 BST" in the dealership's zone. */
function whenText(iso: string, tz: string): string {
  return `${formatDayLabel(formatDate(iso, tz))} ${formatClock(iso, tz)} ${zoneAbbr(tz, iso)}`;
}

/** A failure whose outcome is unknown: the request may have been applied (timeout, 5xx, no answer). */
function outcomeUnknown(err: unknown): boolean {
  return err instanceof NetworkError || (isApiError(err) && err.status >= 500);
}

/**
 * A refusal the server judged (4xx): the booking was not made, so the next try is a new request.
 * 408 (timed out before being judged) and 429 (throttled) are not judgements: a booking saved by
 * an earlier attempt that answered 503 must still replay on the retry, so the key is kept.
 */
function refused(err: unknown): boolean {
  return isApiError(err) && err.status >= 400 && err.status < 500 && err.status !== 408 && err.status !== 429;
}

/**
 * The appointment writes of the app: book, reschedule, and the workshop actions (If-Match). Every
 * success re-reads what it may have changed; a 412 shows the latest version instead of overwriting;
 * a slot refused as taken or against the rules (409/422) re-reads the grid with the pick dropped
 * and says what happened in words.
 */
export function useAppointmentActions(o: Options) {
  const { appointment, setAppointment, form, run, toast, refreshSchedule, refreshLists, timezoneOf, revealPanel } = o;
  // Writes await the server: what they refresh afterwards must be the form as it is then (the user
  // may have picked another day meanwhile), never the render that started the write.
  const latest = useRef({ form, refreshSchedule, refreshLists });
  latest.current = { form, refreshSchedule, refreshLists };

  /** After any successful write: re-read what it may have changed, never reporting a failure. */
  function refreshAfterWrite() {
    const now = latest.current;
    void now.form.refreshAvailability({ silent: true, resetSelection: true });
    void now.refreshSchedule({ silent: true });
    now.refreshLists();
  }

  /** A refused slot: the grid and the first-available list are re-read (the pick dropped), and the board. */
  function refreshAfterConflict() {
    const now = latest.current;
    void now.form.refreshAvailability({ silent: true, resetSelection: true, next: true });
    void now.refreshSchedule({ silent: true });
  }

  /**
   * A write whose outcome is unknown (503, network error): re-read silently what it may have
   * changed, keeping the pick while the grid still offers it (a retry replays the same key).
   */
  function refreshAfterUnknown() {
    const now = latest.current;
    void now.form.refreshAvailability({ silent: true, next: true });
    void now.refreshSchedule({ silent: true });
    now.refreshLists();
  }

  /** Shows a re-read of the panel's appointment, unless the panel moved on (another one, closed, newer). */
  function adopt(fetched: Appointment) {
    setAppointment((shown) => adoptFetched(shown, fetched));
  }

  /** Runs an appointment write (If-Match); true when it succeeded. `move` = a reschedule. */
  async function runAction(fn: AppointmentAction, opts?: ActionOptions & { move?: boolean }): Promise<boolean> {
    if (!appointment) return false;
    const target = appointment;
    const item = await run(async () => {
      try {
        return await fn(target);
      } catch (err) {
        if (isApiError(err) && err.status === 412) {
          // Someone else changed it first: show their version instead of overwriting it.
          adopt(await api.appointment(target.id));
          throw new Error("This appointment was just changed by someone else — showing the latest. Please check and retry.", {
            cause: err,
          });
        }
        if (isSlotConflict(err)) {
          // The state the button was offered for may be stale (checked in elsewhere, a slot taken).
          void api
            .appointment(target.id)
            .then(adopt)
            .catch(() => undefined);
          if (opts?.move) {
            refreshAfterConflict();
            throw new Error(bookingErrorText(err), { cause: err });
          }
          throw new Error(actionErrorText(err), { cause: err });
        }
        if (outcomeUnknown(err)) {
          refreshAfterUnknown();
          void api
            .appointment(target.id)
            .then(adopt)
            .catch(() => undefined);
        }
        throw err;
      }
    }, { write: true });
    if (!item) return false;
    adopt(item);
    toast(opts?.success ?? "Saved.");
    refreshAfterWrite();
    return true;
  }

  async function rescheduleTo(slot: Slot) {
    const tz = timezoneOf(form.dealershipId);
    // The grid was computed with the form's waiter flag: send it, so the move is judged the same way.
    const moved = await runAction((a) => api.reschedule(a, slot.start, form.isWaiter), {
      success: `Rescheduled to ${whenText(slot.start, tz)}.`,
      move: true,
    });
    if (!moved) return;
    // The grid stays and is re-read with the appointment's hold counted again.
    latest.current.form.finishReschedule();
    revealPanel();
  }

  async function confirm() {
    const slot = form.selectedSlot;
    if (!slot || !form.ready) return;
    const request = {
      dealershipId: form.dealershipId,
      vehicleId: form.vehicleId,
      serviceTypeIds: form.serviceTypeIds,
      startAt: slot.start,
      isWaiter: form.isWaiter,
    };
    // Same request (a re-click, a retry after a timeout): same key, so the server replays it.
    const key = form.bookingKey(request);
    const item = await run(async () => {
      try {
        return await api.book(request, key);
      } catch (err) {
        if (refused(err)) latest.current.form.settleBookingKey();
        if (isSlotConflict(err)) {
          refreshAfterConflict();
          throw new Error(bookingErrorText(err), { cause: err });
        }
        if (outcomeUnknown(err)) refreshAfterUnknown();
        throw err;
      }
    }, { write: true });
    if (!item) return;
    latest.current.form.settleBookingKey();
    const tz = timezoneOf(item.dealershipId);
    setAppointment(item);
    latest.current.form.finishReschedule();
    toast(`Booked ${whenText(item.startAt, tz)} — ${item.serviceBayName}, ${item.technicianName}.`);
    revealPanel();
    refreshAfterWrite();
  }

  return { runAction, rescheduleTo, confirm, refreshAfterWrite };
}
