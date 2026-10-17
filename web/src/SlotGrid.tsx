import type { Ref } from "react";
import { slotReasonText } from "./labels";
import { formatClock, formatDayLabel, zoneLabel } from "./time";
import type { Appointment, Availability, NextAvailability, NextSlot, Slot } from "./types";
import { Icon, slotReasonIcon } from "./Icon";
import { EmptyState } from "./Illustration";

type Props = {
  availability: Availability | null;
  selectedSlot: Slot | null;
  busy: boolean;
  loading?: boolean;
  /**
   * Staff see how many bays and technicians are free per start (and why best-fit starts help the
   * workshop); customers only that it is free.
   */
  showCapacity?: boolean;
  onSelect: (slot: Slot) => void;
  onConfirm: () => void;
  /** Moving this appointment: slots ignore its own hold, and the main action reschedules it. */
  rescheduleTarget?: Appointment | null;
  onReschedule?: (slot: Slot) => void;
  onStopReschedule?: () => void;
  sectionRef?: Ref<HTMLElement>;
  /**
   * What Confirm would book, in one line (car and owner, services, dealership, time, waiter): shown
   * next to the button so a wrong car or service is caught before it is booked.
   */
  review?: string;
};

const sameStart = (a: string, b: string) => Date.parse(a) === Date.parse(b);

/** "2 bays · 3 techs" for staff; a customer only needs to know the start is free. */
function capacityText(slot: { freeBays: number; freeTechnicians: number }, show: boolean): string {
  if (!show) return "Free";
  return `${slot.freeBays} bay${slot.freeBays === 1 ? "" : "s"} · ${slot.freeTechnicians} tech${slot.freeTechnicians === 1 ? "" : "s"}`;
}

/** Marks a recommended start: the star is decoration, "Best fit" is what a screen reader hears. */
function BestFitBadge() {
  return (
    <small className="best-fit">
      <span aria-hidden="true">★ </span>Best fit
    </small>
  );
}

/**
 * Says what the star means. Recommended starts pack work next to existing jobs and keep long free
 * stretches whole; every other free start stays bookable.
 */
function BestFitLegend({ staff }: { staff: boolean }) {
  return (
    <p className="hint best-fit-legend">
      <span aria-hidden="true">★ </span>
      {staff
        ? "Best fit: packs the job next to existing work, keeping long free stretches for long jobs."
        : "Best fit: recommended times help the workshop fit more jobs. Any free time can be booked."}
    </p>
  );
}

const recommended = (slot: { recommended?: boolean; available?: boolean }) =>
  Boolean(slot.recommended) && slot.available !== false;

export function SlotGrid({
  availability,
  selectedSlot,
  busy,
  loading = false,
  showCapacity = false,
  onSelect,
  onConfirm,
  rescheduleTarget = null,
  onReschedule,
  onStopReschedule,
  sectionRef,
  review = "",
}: Props) {
  const tz = availability?.timezone ?? "UTC";
  const pickedLabel =
    selectedSlot && availability
      ? `${formatDayLabel(availability.date)} ${formatClock(selectedSlot.start, tz)}`
      : "";
  const unchanged = Boolean(rescheduleTarget && selectedSlot && sameStart(selectedSlot.start, rescheduleTarget.startAt));

  return (
    <section className="panel" ref={sectionRef} aria-busy={loading}>
      <h2>
        <Icon name="clock" />
        2. Slots
        {availability ? (
          <span className="heading-detail">
            {" "}
            · {formatDayLabel(availability.date)} · {zoneLabel(tz, `${availability.date}T12:00:00Z`)}
          </span>
        ) : null}
      </h2>
      {rescheduleTarget ? (
        <p className="hint reschedule-hint">
          <Icon name="reschedule" />
          Rescheduling {rescheduleTarget.serviceName} for {rescheduleTarget.vehicleLabel}. Its current time counts as
          free; pick a new slot (change the date to look at other days).
        </p>
      ) : null}
      {loading ? (
        <p className="hint">Loading slots…</p>
      ) : !availability ? (
        <EmptyState art="search">Press “Check availability” to see free slots for the chosen day.</EmptyState>
      ) : availability.closed ? (
        <EmptyState art="closed">The dealership is closed that day ({availability.closedReason}).</EmptyState>
      ) : availability.reason === "day_too_short" ? (
        <EmptyState art="noSlots">
          Opening hours that day are shorter than the {availability.durationMinutes}-minute job; try another day.
        </EmptyState>
      ) : availability.slots.length === 0 ? (
        <EmptyState art="noSlots">No start times that day.</EmptyState>
      ) : (
        <>
          <div className="slots">
            {availability.slots.map((slot) => {
              const label = formatClock(slot.start, tz);
              const detail = slot.available ? capacityText(slot, showCapacity) : slotReasonText(slot.reason, showCapacity);
              const selected = selectedSlot !== null && sameStart(selectedSlot.start, slot.start);
              const best = recommended(slot);
              return (
                <button
                  key={slot.start}
                  type="button"
                  className={`slot ${slot.available ? "ok" : `busy reason-${slot.reason ?? "unknown"}`} ${
                    best ? "recommended" : ""
                  } ${selected ? "selected" : ""}`}
                  disabled={!slot.available || busy}
                  aria-pressed={selected}
                  aria-label={`${label}, ${slot.available ? "available" : detail}${best ? ", best fit" : ""}`}
                  onClick={() => onSelect(slot)}
                >
                  <span>{label}</span>
                  <small>
                    <Icon name={slot.available ? "check" : slotReasonIcon(slot.reason)} />
                    {detail}
                  </small>
                  {best ? <BestFitBadge /> : null}
                </button>
              );
            })}
          </div>
          {availability.slots.some(recommended) ? <BestFitLegend staff={showCapacity} /> : null}
        </>
      )}
      <div className="actions">
        {rescheduleTarget && onReschedule ? (
          <>
            <button
              type="button"
              className="primary"
              disabled={!selectedSlot || busy || unchanged}
              onClick={() => selectedSlot && onReschedule(selectedSlot)}
            >
              <Icon name="reschedule" />
              {selectedSlot && !unchanged ? `Reschedule to ${pickedLabel}` : "Pick a new time to reschedule"}
            </button>
            <button type="button" disabled={busy} onClick={onStopReschedule}>
              <Icon name="x" />
              Stop rescheduling
            </button>
            <button type="button" className="link" disabled={!selectedSlot || busy} onClick={onConfirm}>
              <Icon name="plus" />
              Book as an extra appointment instead
            </button>
          </>
        ) : (
          <button
            type="button"
            className="primary"
            disabled={!selectedSlot || busy}
            aria-describedby={selectedSlot && review ? "booking-review" : undefined}
            onClick={onConfirm}
          >
            <Icon name="check" />
            {selectedSlot ? `Confirm booking · ${pickedLabel}` : "Confirm booking"}
          </button>
        )}
      </div>
      {selectedSlot && review ? (
        <p id="booking-review" className="hint booking-review">
          {rescheduleTarget ? "Extra appointment: " : "You are booking: "}
          {review}
        </p>
      ) : null}
    </section>
  );
}

type NextProps = {
  next: NextAvailability | null;
  selectedSlot: Slot | null;
  busy: boolean;
  showCapacity?: boolean;
  onPick: (slot: NextSlot) => void;
};

/** Groups the list's starts by day (they come in time order, so each day's run is contiguous). */
function byDay(slots: NextSlot[]): { date: string; slots: NextSlot[] }[] {
  const days: { date: string; slots: NextSlot[] }[] = [];
  for (const slot of slots) {
    const last = days.at(-1);
    if (last?.date === slot.date) last.slots.push(slot);
    else days.push({ date: slot.date, slots: [slot] });
  }
  return days;
}

/**
 * A few free starts over the coming days, grouped by day; picking one fills the grid. Staff see
 * the earliest ("Next available"); customers each day's best fits first ("Suggested times").
 */
export function NextSlotList({ next, selectedSlot, busy, showCapacity = false, onPick }: NextProps) {
  if (!next) return null;
  if (next.slots.length === 0) {
    return (
      <EmptyState art="noSlots">
        No free slot between {formatDayLabel(next.searchedFrom)} and {formatDayLabel(next.searchedTo)}.
      </EmptyState>
    );
  }
  return (
    <div className="next-slots">
      <h3>
        <Icon name="zap" />
        {showCapacity ? "Next available" : "Suggested times"}{" "}
        <span className="hint">
          · {next.durationMinutes} min · {zoneLabel(next.timezone, next.slots[0].start)}
        </span>
      </h3>
      <div className="next-days">
        {byDay(next.slots).map((day) => {
          const label = formatDayLabel(day.date);
          return (
            <div key={day.date} className="next-day" role="group" aria-label={label}>
              <h4>
                <Icon name="calendar" />
                {label}
              </h4>
              <div className="slots">
                {day.slots.map((slot) => {
                  const selected = selectedSlot !== null && sameStart(selectedSlot.start, slot.start);
                  const clock = formatClock(slot.start, next.timezone);
                  return (
                    <button
                      key={slot.start}
                      type="button"
                      className={`slot ok ${slot.recommended ? "recommended" : ""} ${selected ? "selected" : ""}`}
                      disabled={busy}
                      aria-pressed={selected}
                      aria-label={`${label} ${clock}, ${capacityText(slot, showCapacity).toLowerCase()}${slot.recommended ? ", best fit" : ""}`}
                      onClick={() => onPick(slot)}
                    >
                      <span>{clock}</span>
                      <small>
                        <Icon name="check" />
                        {capacityText(slot, showCapacity)}
                      </small>
                      {slot.recommended ? <BestFitBadge /> : null}
                    </button>
                  );
                })}
              </div>
            </div>
          );
        })}
      </div>
      {next.slots.some(recommended) ? <BestFitLegend staff={showCapacity} /> : null}
    </div>
  );
}
