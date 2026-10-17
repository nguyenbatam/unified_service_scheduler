import { useEffect, useState } from "react";
import { confirmAction } from "./ConfirmDialog";
import { ErrorBanner } from "./ErrorBanner";
import { adminErrorText } from "./admin";
import { api, isApiError } from "./api";
import { errorText } from "./errors";
import { formatClock, formatDate, zonedToIso } from "./time";
import { useAsyncAction } from "./useAsyncAction";
import type { Dealership, TechnicianAdmin, TechnicianRoster, TechnicianShift } from "./types";
import { Icon } from "./Icon";

type Props = {
  technicians: TechnicianAdmin[];
  dealerships: Dealership[];
  /** Roster changes bump the technician version. */
  onSaved: (roster: TechnicianRoster) => void;
};

type ShiftRow = { on: boolean; startTime: string; endTime: string; breakStart: string; breakEnd: string };

const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const WEEKDAY_FULL = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"];
const TIME_LABEL = { startTime: "starts", endTime: "ends", breakStart: "break starts", breakEnd: "break ends" } as const;

function shiftRows(roster: TechnicianRoster | null, d: Dealership | undefined): ShiftRow[] {
  return WEEKDAYS.map((_, i) => {
    const s = roster?.shifts.find((x) => x.weekday === i + 1);
    return {
      on: Boolean(s),
      startTime: s?.startTime ?? d?.openTime ?? "08:00",
      endTime: s?.endTime ?? d?.closeTime ?? "17:00",
      breakStart: s?.breakStart ?? "",
      breakEnd: s?.breakEnd ?? "",
    };
  });
}

function toShifts(rows: ShiftRow[]): TechnicianShift[] {
  return rows.flatMap((r, i) =>
    r.on
      ? [
          {
            weekday: i + 1,
            startTime: r.startTime,
            endTime: r.endTime,
            ...(r.breakStart && r.breakEnd ? { breakStart: r.breakStart, breakEnd: r.breakEnd } : {}),
          },
        ]
      : [],
  );
}

/** Admin: one technician's weekly shifts and dated absences (times in the dealership's zone). */
export function RosterEditor({ technicians, dealerships, onSaved }: Props) {
  const [techId, setTechId] = useState("");
  const [roster, setRoster] = useState<TechnicianRoster | null>(null);
  const [rows, setRows] = useState<ShiftRow[]>(() => shiftRows(null, undefined));
  const [absStart, setAbsStart] = useState("");
  const [absEnd, setAbsEnd] = useState("");
  const [absReason, setAbsReason] = useState("");
  const { busy, error, setError, run } = useAsyncAction();

  const tech = technicians.find((t) => t.id === techId);
  const dealer = dealerships.find((d) => d.id === tech?.dealershipId);
  const tz = dealer?.timezone ?? "UTC";

  useEffect(() => {
    // Also when the list changes under it (another dealership picked).
    if (technicians[0] && !technicians.some((t) => t.id === techId)) setTechId(technicians[0].id);
  }, [techId, technicians]);

  // Load on selection, and again when the technician changed elsewhere (activate/deactivate).
  useEffect(() => {
    if (!tech || (roster?.technicianId === tech.id && roster.version >= tech.version)) return;
    let cancelled = false;
    api
      .roster(tech.id)
      .then((r) => {
        if (!cancelled) setRoster(r);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorText(err));
      });
    return () => {
      cancelled = true;
    };
  }, [tech?.id, tech?.version]);

  useEffect(() => setRows(shiftRows(roster, dealer)), [roster, dealer?.id]);

  function apply(r: TechnicianRoster) {
    setRoster(r);
    onSaved(r);
  }

  /** Sends the shown roster version as If-Match; resolves true when the change was saved. */
  async function guarded(fn: (version: number) => Promise<TechnicianRoster>): Promise<boolean> {
    if (!tech || !roster) return false;
    const saved = await run(async () => {
      try {
        apply(await fn(roster.version));
        return true;
      } catch (err) {
        if (isApiError(err) && err.status === 412) {
          apply(await api.roster(tech.id));
          throw new Error("This roster was just changed by someone else — showing the latest. Please retry.");
        }
        throw new Error(adminErrorText(err));
      }
    });
    return saved === true;
  }

  function setRow(i: number, patch: Partial<ShiftRow>) {
    setRows((prev) => prev.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  }

  async function clearShifts() {
    if (
      !(await confirmAction(
        `Remove all of ${tech?.name ?? "this technician"}'s shifts? They will work the dealership's opening hours on every open day.`,
      ))
    ) {
      return;
    }
    await guarded((v) => api.putShifts(techId, v, []));
  }

  async function deleteAbsence(id: string, label: string) {
    if (!(await confirmAction(`Delete the absence ${label}? ${tech?.name ?? "The technician"} becomes bookable then.`))) return;
    await guarded((v) => api.deleteAbsence(techId, v, id));
  }

  async function addAbsence() {
    if (absEnd <= absStart) {
      setError("The absence must end after it starts.");
      return;
    }
    const saved = await guarded((version) =>
      api.addAbsence(techId, version, {
        startsAt: zonedToIso(absStart, tz),
        endsAt: zonedToIso(absEnd, tz),
        reason: absReason.trim() || undefined,
      }),
    );
    if (saved) setAbsReason("");
  }

  return (
    <div>
      <h3>
        <Icon name="users" />
        Technician roster
      </h3>
      <ErrorBanner message={error} />
      <label>
        <span>
          <Icon name="hardHat" />
          Technician
        </span>
        <select value={techId} onChange={(e) => setTechId(e.target.value)}>
          {technicians.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
              {t.active ? "" : " (inactive)"}
            </option>
          ))}
        </select>
      </label>
      {!roster || roster.technicianId !== techId ? (
        error ? null : <p className="hint">Loading roster…</p>
      ) : (
        <>
          <p className="hint">
            {roster.shifts.length === 0
              ? "No shifts set: works the dealership's opening hours."
              : "Works only the ticked days below."}{" "}
            Times are {tz}.
          </p>
          <table className="roster-table">
            <thead>
              <tr>
                <th>Day</th>
                <th>Works</th>
                <th>Start</th>
                <th>End</th>
                <th>Break start</th>
                <th>Break end</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={WEEKDAYS[i]}>
                  <td>{WEEKDAYS[i]}</td>
                  <td>
                    <input
                      type="checkbox"
                      aria-label={`Works on ${WEEKDAY_FULL[i]}`}
                      checked={r.on}
                      onChange={(e) => setRow(i, { on: e.target.checked })}
                    />
                  </td>
                  {(["startTime", "endTime", "breakStart", "breakEnd"] as const).map((k) => (
                    <td key={k}>
                      <input
                        type="time"
                        aria-label={`${WEEKDAY_FULL[i]} ${TIME_LABEL[k]}`}
                        value={r[k]}
                        disabled={!r.on}
                        onChange={(e) => setRow(i, { [k]: e.target.value })}
                      />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
          <div className="actions">
            <button
              type="button"
              className="primary"
              disabled={busy}
              onClick={() => guarded((v) => api.putShifts(techId, v, toShifts(rows)))}
            >
              <Icon name="check" />
              Save shifts
            </button>{" "}
            <button type="button" disabled={busy || roster.shifts.length === 0} onClick={clearShifts}>
              <Icon name="clock" />
              Use opening hours
            </button>
          </div>

          <h3>
            <Icon name="calendar" />
            Absences
          </h3>
          {roster.absences.length === 0 ? <p className="hint">No upcoming absences.</p> : null}
          <div className="tech-list">
            {roster.absences.map((a) => {
              const label = `${formatDate(a.startsAt, tz)} ${formatClock(a.startsAt, tz)} → ${formatDate(a.endsAt, tz)} ${formatClock(a.endsAt, tz)}`;
              return (
                <div key={a.id} className="tech-row">
                  <span>
                    {label}
                    {a.reason ? ` · ${a.reason}` : ""}
                  </span>
                  <button type="button" disabled={busy} onClick={() => deleteAbsence(a.id, label)}>
                    <Icon name="trash" />
                    Delete
                  </button>
                </div>
              );
            })}
          </div>
          <div className="inline-form">
            <label>
              <span>
                <Icon name="calendar" />
                From
              </span>
              <input type="datetime-local" value={absStart} onChange={(e) => setAbsStart(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="calendar" />
                Until
              </span>
              <input type="datetime-local" value={absEnd} onChange={(e) => setAbsEnd(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="info" />
                Reason
              </span>
              <input type="text" value={absReason} maxLength={200} onChange={(e) => setAbsReason(e.target.value)} />
            </label>
            <button type="button" disabled={busy || !absStart || !absEnd} onClick={addAbsence}>
              <Icon name="plus" />
              Add absence
            </button>
          </div>
        </>
      )}
    </div>
  );
}
