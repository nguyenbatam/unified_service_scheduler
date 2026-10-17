import { useEffect, useState } from "react";
import type { ActionOptions } from "./AppointmentPanel";
import { MAX_PAGE_LIMIT, api, type AppointmentAction } from "./api";
import { errorText } from "./errors";
import { eventLabel, skillLevelLabel, statusLabel } from "./labels";
import { formatClock, formatClockRange, formatDate } from "./time";
import { usePagedList } from "./usePagedList";
import {
  Status,
  bayName,
  skillName,
  type Appointment,
  type AppointmentChange,
  type Schedule,
} from "./types";
import { Icon, bayIcon } from "./Icon";

type Props = {
  appointment: Appointment;
  timezone: string;
  busy: boolean;
  /**
   * The board's schedule when it is of this appointment's dealership; null otherwise (another
   * dealership, or still loading): the bays and technicians are then read for the appointment itself.
   */
  schedule: Schedule | null;
  onAction: (fn: AppointmentAction, opts?: ActionOptions) => void;
};

/** Audit-trail page size. */
const HISTORY_PAGE = 50;

/** Only work that has not started can move to another technician or bay (same time window). */
const REASSIGNABLE = new Set<number>([Status.Confirmed, Status.CheckedIn]);

/** Staff-only: reassign technician/bay (If-Match via onAction) and the appointment's audit trail. */
export function AppointmentStaffTools({ appointment, timezone, busy, schedule: boardSchedule, onAction }: Props) {
  const [techId, setTechId] = useState(appointment.technicianId);
  const [bayId, setBayId] = useState(appointment.serviceBayId);
  const [showHistory, setShowHistory] = useState(false);
  // The trail a page at a time (oldest first); a newer version re-reads it from the first page.
  const history = usePagedList<AppointmentChange>(
    (cursor, limit) => api.history(appointment.id, { limit, cursor }),
    appointment.id,
    { pageSize: HISTORY_PAGE, maxPageSize: MAX_PAGE_LIMIT, enabled: showHistory, reloadKey: appointment.version, idOf: (h) => String(h.version) },
  );

  // A newer version (ours, a live update or a 412 reload) resets the form to what is booked.
  useEffect(() => {
    setTechId(appointment.technicianId);
    setBayId(appointment.serviceBayId);
  }, [appointment.id, appointment.version, appointment.technicianId, appointment.serviceBayId]);

  const reassignable = REASSIGNABLE.has(appointment.status);
  const day = formatDate(appointment.startAt, timezone);
  // Not the board's dealership (a group admin looking up a customer's bookings): read that
  // dealership's bays and technicians, so reassigning never silently disappears.
  const [own, setOwn] = useState<{ key: string; schedule: Schedule | null; error: string } | null>(null);
  const ownKey = `${appointment.dealershipId}/${day}`;
  const needOwn = boardSchedule === null && reassignable;
  useEffect(() => {
    if (!needOwn) return;
    let cancelled = false;
    api
      .schedule(appointment.dealershipId, day)
      .then((s) => {
        if (!cancelled) setOwn({ key: ownKey, schedule: s, error: "" });
      })
      .catch((err: unknown) => {
        if (!cancelled) setOwn({ key: ownKey, schedule: null, error: errorText(err) });
      });
    return () => {
      cancelled = true;
    };
  }, [needOwn, ownKey]);
  const fetched = own?.key === ownKey ? own : null;
  const schedule = boardSchedule ?? fetched?.schedule ?? null;

  const techs = schedule?.technicians ?? [];
  const bays = schedule?.bays ?? [];
  const techName = (id: string) =>
    techs.find((t) => t.id === id)?.name ?? (id === appointment.technicianId ? appointment.technicianName : id.slice(0, 8));
  const bayLabel = (id: string) =>
    bays.find((b) => b.id === id)?.name ?? (id === appointment.serviceBayId ? appointment.serviceBayName : id.slice(0, 8));
  const changed = techId !== appointment.technicianId || bayId !== appointment.serviceBayId;
  const canReassign = schedule !== null && reassignable;

  function reassign() {
    onAction(
      (a) =>
        api.reassign(a, {
          technicianId: techId !== a.technicianId ? techId : undefined,
          serviceBayId: bayId !== a.serviceBayId ? bayId : undefined,
        }),
      { success: `Reassigned to ${bayLabel(bayId)} / ${techName(techId)}.` },
    );
  }

  return (
    <>
      {canReassign ? (
        <div className="inline-form">
          <label>
            <span>
              <Icon name="hardHat" />
              Technician
            </span>
            <select value={techId} onChange={(e) => setTechId(e.target.value)}>
              {techs.some((t) => t.id === appointment.technicianId) ? null : (
                <option value={appointment.technicianId}>{appointment.technicianName}</option>
              )}
              {techs.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                  {t.skillLevel ? ` · ${skillLevelLabel(t.skillLevel)}` : ""}
                  {t.skills?.length ? ` · ${t.skills.map(skillName).join(", ")}` : ""}
                </option>
              ))}
            </select>
          </label>
          <label>
            <span>
              <Icon name={bayIcon(bays.find((b) => b.id === bayId)?.bayType ?? appointment.serviceBayType)} />
              Bay
            </span>
            <select value={bayId} onChange={(e) => setBayId(e.target.value)}>
              {bays.some((b) => b.id === appointment.serviceBayId) ? null : (
                <option value={appointment.serviceBayId}>{appointment.serviceBayName}</option>
              )}
              {bays.map((b) => (
                <option key={b.id} value={b.id}>
                  {b.name} ({bayName(b.bayType)})
                </option>
              ))}
            </select>
          </label>
          <button type="button" disabled={busy || !changed} onClick={reassign}>
            <Icon name="reassign" />
            Reassign
          </button>
        </div>
      ) : reassignable ? (
        <p className="hint">
          {fetched?.error
            ? `Reassigning needs this dealership's bays and technicians, which could not be loaded: ${fetched.error}`
            : "Loading bays and technicians for reassigning…"}
        </p>
      ) : null}
      <div className="actions">
        <button type="button" onClick={() => setShowHistory((v) => !v)}>
          <Icon name="history" />
          {showHistory ? "Hide history" : "History"}
        </button>
      </div>
      {showHistory ? (
        history.error ? (
          <p className="hint">{history.error}</p>
        ) : !history.items ? (
          <p className="hint">Loading history…</p>
        ) : (
          <>
            <ol className="history">
              {history.items.map((h) => (
                <li key={`${h.version}-${h.eventType}`}>
                  <strong>v{h.version}</strong> {eventLabel(h.eventType)} ·{" "}
                  {formatDate(h.at, timezone)} {formatClock(h.at, timezone)} · {statusLabel(h.status)} ·{" "}
                  {formatDate(h.startAt, timezone)} {formatClockRange(h.startAt, h.endAt, timezone)} ·{" "}
                  {bayLabel(h.serviceBayId)} / {techName(h.technicianId)}
                </li>
              ))}
            </ol>
            {history.hasMore ? (
              <div className="actions">
                <button type="button" disabled={history.loading} onClick={() => void history.more()}>
                  <Icon name="chevronDown" />
                  {history.loading ? "Loading…" : "Show later changes"}
                </button>
              </div>
            ) : null}
          </>
        )
      ) : null}
    </>
  );
}
