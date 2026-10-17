import type { CSSProperties } from "react";
import { statusLabel } from "./labels";
import {
  boardHours,
  boardLayout,
  boardOffset,
  clockOf,
  formatClock,
  formatClockRange,
  formatDate,
  formatDayLabel,
  minutesOnDay,
  parseClockMinutes,
  zoneLabel,
} from "./time";
import { Status, bayName, bayPurpose, statusName, type Appointment, type Schedule } from "./types";
import { useNow } from "./useNow";
import { Icon, bayIcon, statusIcon } from "./Icon";
import { EmptyState } from "./Illustration";

type Props = {
  schedule: Schedule | null;
  /** The last load failed (shown with a retry instead of an endless "Loading"). */
  error?: string;
  onRetry?: () => void;
  /** The board's day (YYYY-MM-DD), named in the heading while the schedule loads. */
  date?: string;
  selectedId?: string;
  /** Opens the appointment panel (actions, reassign, history). */
  onSelect: (appointment: Appointment) => void;
};

/** Finished work no longer holds its bay: drawn faded and narrow so a later booking stays readable. */
const FINISHED = new Set<number>([Status.Completed, Status.NoShow]);

/** Blocks shorter than this (visible minutes) get one line: time and status. */
const COMPACT_MINUTES = 45;
/** Blocks at least this long also name the technician. */
const FULL_MINUTES = 75;

/** Statuses drawn on the board, for the colour legend. */
const LEGEND = [Status.Confirmed, Status.CheckedIn, Status.InProgress, Status.Completed, Status.NoShow];

/** Past every board: a flagged overrun holds its bay until it is completed. */
const UNTIL_COMPLETED = "9999-12-31T00:00:00Z";

/**
 * Where a block ends on the board: completed work ends when it was completed, if that was earlier;
 * a flagged overrun (in progress, overrunAt set) runs to the bottom of every board until it is
 * completed, as the server holds its bay and technician (a carry-over job shows on the next day too).
 */
function visualEnd(a: Appointment): string {
  if (a.status === Status.InProgress && a.overrunAt) return UNTIL_COMPLETED;
  if (a.status === Status.Completed && a.completedAt) {
    const done = Date.parse(a.completedAt);
    if (done > Date.parse(a.startAt) && done < Date.parse(a.endAt)) return a.completedAt;
  }
  return a.endAt;
}

/** Status colours, lunch and the now line, so the board reads without hovering. */
function BoardLegend() {
  return (
    <ul className="board-key" aria-label="Board legend">
      {LEGEND.map((s) => (
        <li key={s}>
          <span className={`swatch appt-swatch status-${statusName(s)}`} aria-hidden="true">
            <Icon name={statusIcon(s)} />
          </span>
          {statusLabel(s)}
        </li>
      ))}
      <li>
        <span className="swatch lunch-swatch" aria-hidden="true" />
        <Icon name="coffee" />
        Lunch
      </li>
      <li>
        <span className="swatch now-swatch" aria-hidden="true" />
        Now
      </li>
    </ul>
  );
}

/**
 * One dealership's day: a column per bay, the track as tall as the day is long (CSS --board-minutes
 * × --board-px-per-minute, so a 30-minute job stays readable), and appointment blocks that show as
 * much as fits: one line (time · status) when short, then service, then technician.
 */
export function DayBoard({ schedule, error, onRetry, date: day, selectedId, onSelect }: Props) {
  const now = useNow();
  if (!schedule) {
    return (
      <section className="panel">
        <h2>
          <Icon name="board" />
          Day board{day ? <span className="heading-detail"> · {formatDayLabel(day)}</span> : null}</h2>
        {error ? (
          <div className="banner error" role="alert">
            Could not load the day board: {error}{" "}
            {onRetry ? (
              <button type="button" className="link" onClick={onRetry}>
                <Icon name="refresh" />
                Retry
              </button>
            ) : null}
          </div>
        ) : (
          <p className="hint">Loading the day board…</p>
        )}
      </section>
    );
  }
  const { openTime, closeTime, timezone, date } = schedule;
  const hours = boardHours(openTime, closeTime);
  const span = Math.max(parseClockMinutes(closeTime) - parseClockMinutes(openTime), 60);
  const lunchTop = schedule.lunchStart ? boardOffset(schedule.lunchStart, openTime, closeTime) : null;
  const lunchBottom = schedule.lunchEnd ? boardOffset(schedule.lunchEnd, openTime, closeTime) : null;
  const nowIso = new Date(now).toISOString();
  const nowTop = formatDate(nowIso, timezone) === date ? boardOffset(minutesOnDay(nowIso, date, timezone), openTime, closeTime) : null;
  const visible = schedule.appointments.filter((a) => a.status !== Status.Cancelled);
  const trackStyle = { "--board-minutes": span } as CSSProperties;

  return (
    <section className="panel">
      <h2>
        <Icon name="board" />
        Day board{" "}
        <span className="heading-detail">
          · {formatDayLabel(date)} · {openTime}–{closeTime} · {zoneLabel(timezone, `${date}T12:00:00Z`)}
        </span>
      </h2>
      {schedule.closed && <p className="hint">Closed that day ({schedule.closedReason}).</p>}
      {schedule.bays.length === 0 ? (
        <EmptyState art="garage">This dealership has no bays yet.</EmptyState>
      ) : (
        <div className="board-wrap" style={trackStyle}>
          <div className="board-axis" aria-hidden="true">
            {hours.map((m) => (
              <span key={m} style={{ top: `${boardOffset(m, openTime, closeTime) ?? 0}%` }}>
                {clockOf(m)}
              </span>
            ))}
          </div>
          <div className="board">
            {schedule.bays.map((bay) => (
              <div key={bay.id} className="bay-col">
                <header title={`${bayName(bay.bayType)} bay: ${bayPurpose(bay.bayType)}`}>
                  <strong>
                    <Icon name={bayIcon(bay.bayType)} />
                    {bay.name}
                  </strong>
                  <span>{bayName(bay.bayType)}</span>
                </header>
                <div className="bay-track">
                  {hours.map((m) => (
                    <div key={m} className="hour-line" style={{ top: `${boardOffset(m, openTime, closeTime) ?? 0}%` }} />
                  ))}
                  {lunchTop !== null && lunchBottom !== null ? (
                    <div
                      className="lunch-band"
                      style={{ top: `${lunchTop}%`, height: `${lunchBottom - lunchTop}%` }}
                      title={`Lunch ${schedule.lunchStart}–${schedule.lunchEnd}`}
                    />
                  ) : null}
                  {nowTop !== null ? <div className="now-line" style={{ top: `${nowTop}%` }} title="Now" /> : null}
                  {visible
                    .filter((a) => a.serviceBayId === bay.id)
                    .map((a) => {
                      const layout = boardLayout(a.startAt, visualEnd(a), date, openTime, closeTime, timezone);
                      const finished = FINISHED.has(a.status);
                      const compact = layout.minutes < COMPACT_MINUTES;
                      const label = `${statusLabel(a.status)}${a.overrunAt ? " · overrun" : ""}`;
                      const classes = [
                        "appt",
                        `status-${statusName(a.status)}`,
                        finished ? "finished" : "",
                        compact ? "compact" : "",
                        layout.clippedStart ? "clip-start" : "",
                        layout.clippedEnd ? "clip-end" : "",
                        a.id === selectedId ? "selected" : "",
                      ];
                      return (
                        <button
                          type="button"
                          key={a.id}
                          className={classes.filter(Boolean).join(" ")}
                          onClick={() => onSelect(a)}
                          style={{ top: `${layout.topPct}%`, height: `${layout.heightPct}%` }}
                          title={`${label} · ${formatClockRange(a.startAt, a.endAt, timezone)} · ${a.serviceName} · ${a.technicianName}${
                            a.isWaiter ? " · waiting on site" : ""
                          }`}
                        >
                          {compact ? (
                            <span className="appt-line">
                              <strong>{formatClock(a.startAt, timezone)}</strong> · <Icon name={statusIcon(a.status)} />
                              {label}
                            </span>
                          ) : (
                            <>
                              <strong className="appt-line">
                                {formatClockRange(a.startAt, a.endAt, timezone)}
                                {a.isWaiter ? (
                                  <>
                                    {" · "}
                                    <Icon name="armchair" />
                                    Waiter
                                  </>
                                ) : null}
                              </strong>
                              <span className="status-badge appt-line">
                                <Icon name={statusIcon(a.status)} />
                                {label}
                              </span>
                              <span className="appt-line">
                                {a.lines.length > 1 ? `${a.lines.length} jobs: ` : ""}
                                {a.serviceName}
                              </span>
                              {layout.minutes >= FULL_MINUTES ? <span className="appt-line">{a.technicianName}</span> : null}
                            </>
                          )}
                        </button>
                      );
                    })}
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
      <BoardLegend />
      <p className="hint board-legend">
        Faded blocks are finished (completed or no-show) and no longer hold the bay. A cut edge means the job
        continues before opening or after closing. Hover or tap a block for its details.
      </p>
    </section>
  );
}
