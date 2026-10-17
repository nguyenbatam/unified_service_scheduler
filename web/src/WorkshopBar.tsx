import { TruncatedHint } from "./TruncatedHint";
import { dealershipToday, nextDealershipMidnight } from "./booking";
import { formatDayLabel, shiftDay, zoneLabel } from "./time";
import type { Dealership } from "./types";
import { useNow } from "./useNow";
import { Icon } from "./Icon";

type DayNavProps = { date: string; today?: string; onDateChange: (day: string) => void };

/** Previous / today / next day, plus a date field. */
export function DayNav({ date, today, onDateChange }: DayNavProps) {
  return (
    <div className="day-nav" role="group" aria-label="Board day">
      <button type="button" onClick={() => onDateChange(shiftDay(date, -1))}>
        <Icon name="chevronLeft" />
        Previous day
      </button>
      <button type="button" disabled={!today || today === date} onClick={() => today && onDateChange(today)}>
        <Icon name="calendar" />
        Today
      </button>
      <button type="button" onClick={() => onDateChange(shiftDay(date, 1))}>
        Next day
        <Icon name="chevronRight" className="icon-end" />
      </button>
      <label>
        <span className="visually-hidden">Board date</span>
        <input type="date" value={date} onChange={(e) => e.target.value && onDateChange(e.target.value)} />
      </label>
    </div>
  );
}

type Props = {
  dealerships: Dealership[];
  dealershipId: string;
  onDealershipChange: (id: string) => void;
  /** Locked while rescheduling (the move stays at the appointment's dealership). */
  dealershipLocked?: boolean;
  /** The board's own day: moving it never touches the booking form or the picked slot. */
  date: string;
  onDateChange: (day: string) => void;
  /** The dealership list stopped at FETCH_ALL_MAX. */
  truncated?: boolean;
};

/**
 * Staff context at the top of the workshop view: which dealership the board, the booking form and
 * the live feed follow, and which day the board shows.
 */
export function WorkshopBar({ dealerships, dealershipId, onDealershipChange, dealershipLocked = false, date, onDateChange, truncated = false }: Props) {
  const dealership = dealerships.find((d) => d.id === dealershipId);
  // "Today" follows the clock in the dealership's zone (re-read at its midnight), not the day the
  // catalog was loaded on: a board left open overnight still jumps to the real today.
  const now = useNow(dealership ? [nextDealershipMidnight(dealership)] : []);
  const today = dealership ? dealershipToday(dealership, now) : undefined;
  return (
    <section className="panel workshop-bar" aria-label="Workshop">
      <label className="workshop-dealer">
        <span>
          <Icon name="dealership" />
          Dealership
        </span>
        <select value={dealershipId} disabled={dealershipLocked} onChange={(e) => onDealershipChange(e.target.value)}>
          {dealerships.map((d) => (
            <option key={d.id} value={d.id}>
              {d.name} ({d.openTime}–{d.closeTime})
            </option>
          ))}
        </select>
      </label>
      <TruncatedHint shown={truncated} what="dealerships" />
      {date ? <DayNav date={date} today={today} onDateChange={onDateChange} /> : null}
      {dealership ? (
        <p className="hint workshop-detail">
          <Icon name="clock" />
          {date ? `${formatDayLabel(date)} · ` : ""}times in {zoneLabel(dealership.timezone)}
          {dealershipLocked ? " · rescheduling: the dealership stays as booked" : ""}
        </p>
      ) : null}
    </section>
  );
}
