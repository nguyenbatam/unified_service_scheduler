import { useCallback, useEffect, useState } from "react";
import { AdminNotice, StaleDraftNotice } from "./AdminNotice";
import { confirmAction } from "./ConfirmDialog";
import { ErrorBanner } from "./ErrorBanner";
import { WEEKDAY_NAMES, toWeekly, weekProblem, weekRows, type WeekRow } from "./admin";
import { api } from "./api";
import { errorText } from "./errors";
import type { Dealership, DealershipCalendar } from "./types";
import { useAdminAction } from "./useAdminAction";
import { useDraft } from "./useDraft";
import { Icon } from "./Icon";

type Props = {
  dealer: Dealership;
  /** Calendar edits bump the dealership version: reload the catalog so other forms stay current. */
  onChanged: () => Promise<unknown> | void;
};

/** Admin: weekday hour overrides and dated closures (public holidays) of one dealership. */
export function CalendarEditor({ dealer, onChanged }: Props) {
  const [cal, setCal] = useState<DealershipCalendar | null>(null);
  const [closureDate, setClosureDate] = useState("");
  const [closureReason, setClosureReason] = useState("");

  const reload = useCallback(async () => setCal(await api.calendar(dealer.id)), [dealer.id]);
  const { busy, error, setError, notice, save } = useAdminAction(reload);

  // Load on selection, and again when the dealership changed elsewhere (its version moved on).
  useEffect(() => {
    if (cal?.dealershipId === dealer.id && cal.version >= dealer.version) return;
    let cancelled = false;
    api
      .calendar(dealer.id)
      .then((c) => {
        if (!cancelled) setCal(c);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorText(err));
      });
    return () => {
      cancelled = true;
    };
  }, [dealer.id, dealer.version]);

  // The weekly rows are a draft: a live change elsewhere replaces them only while they have no
  // unsaved edits (else "changed elsewhere — reload"); saving sends the calendar version they
  // started from, so a stale draft is refused (412) instead of overwriting the other change.
  const loaded = cal?.dealershipId === dealer.id ? cal : null;
  const draft = useDraft<WeekRow[]>(weekRows(loaded, dealer), loaded?.version ?? 0, `${dealer.id}:${loaded ? "cal" : "none"}`);
  const rows = draft.value;

  function setRow(i: number, patch: Partial<WeekRow>) {
    draft.set((prev) => prev.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  }

  async function apply(c: DealershipCalendar) {
    setCal(c);
    await onChanged();
  }

  async function saveWeekly() {
    if (!cal) return;
    const problem = weekProblem(rows);
    if (problem) {
      setError(problem);
      return;
    }
    await save(async () => {
      const saved = await api.putWeeklyHours(dealer.id, draft.version, toWeekly(rows));
      draft.rebase(weekRows(saved, dealer), saved.version);
      await apply(saved);
    }, "Weekly hours saved.");
  }

  async function addClosure() {
    if (!cal || !closureDate) return;
    const saved = await save(
      async () =>
        apply(
          await api.addClosure(dealer.id, cal.version, {
            date: closureDate,
            ...(closureReason.trim() ? { reason: closureReason.trim() } : {}),
          }),
        ),
      `Closed on ${closureDate}.`,
    );
    if (saved) {
      setClosureDate("");
      setClosureReason("");
    }
  }

  async function reopen(date: string) {
    if (!cal) return;
    if (!(await confirmAction(`Reopen ${dealer.name} on ${date}? Customers will be able to book that day again.`))) return;
    await save(async () => apply(await api.deleteClosure(dealer.id, cal.version, date)), `Reopened on ${date}.`);
  }

  if (!cal || cal.dealershipId !== dealer.id) {
    return (
      <div>
        <h3>
          <Icon name="calendar" />
          Calendar
        </h3>
        <ErrorBanner message={error} />
        {error ? null : <p className="hint">Loading calendar…</p>}
      </div>
    );
  }

  return (
    <div>
      <h3>
        <Icon name="calendar" />
        Calendar
      </h3>
      <p className="hint">
        Weekdays on “Default” use {dealer.openTime}–{dealer.closeTime}
        {dealer.lunchStart ? ` with lunch ${dealer.lunchStart}–${dealer.lunchEnd}` : ""}. Times are {dealer.timezone}.
        Changes that would strand upcoming bookings are refused.
      </p>
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />
      {draft.stale ? <StaleDraftNotice what="The weekly hours were" onReload={draft.reload} /> : null}
      <table className="roster-table">
        <thead>
          <tr>
            <th>Day</th>
            <th>Hours</th>
            <th>Opens</th>
            <th>Closes</th>
            <th>Lunch</th>
            <th>Lunch starts</th>
            <th>Lunch ends</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => {
            const day = WEEKDAY_NAMES[i];
            const custom = r.mode === "custom";
            return (
              <tr key={day}>
                <td>{day}</td>
                <td>
                  <select
                    aria-label={`${day} hours`}
                    value={r.mode}
                    onChange={(e) => setRow(i, { mode: e.target.value as WeekRow["mode"] })}
                  >
                    <option value="default">Default</option>
                    <option value="custom">Other hours</option>
                    <option value="closed">Closed</option>
                  </select>
                </td>
                <td>
                  <input
                    type="time"
                    aria-label={`${day} opens`}
                    value={r.openTime}
                    disabled={!custom}
                    onChange={(e) => setRow(i, { openTime: e.target.value })}
                  />
                </td>
                <td>
                  <input
                    type="time"
                    aria-label={`${day} closes`}
                    value={r.closeTime}
                    disabled={!custom}
                    onChange={(e) => setRow(i, { closeTime: e.target.value })}
                  />
                </td>
                <td>
                  <input
                    type="checkbox"
                    aria-label={`${day} has a lunch break`}
                    checked={r.lunch}
                    disabled={!custom}
                    onChange={(e) => setRow(i, { lunch: e.target.checked })}
                  />
                </td>
                <td>
                  <input
                    type="time"
                    aria-label={`${day} lunch starts`}
                    value={r.lunchStart}
                    disabled={!custom || !r.lunch}
                    onChange={(e) => setRow(i, { lunchStart: e.target.value })}
                  />
                </td>
                <td>
                  <input
                    type="time"
                    aria-label={`${day} lunch ends`}
                    value={r.lunchEnd}
                    disabled={!custom || !r.lunch}
                    onChange={(e) => setRow(i, { lunchEnd: e.target.value })}
                  />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <div className="actions">
        <button type="button" className="primary" disabled={busy} onClick={saveWeekly}>
          <Icon name="check" />
          {busy ? "Saving…" : "Save weekly hours"}
        </button>
      </div>

      <h3>
        <Icon name="lock" />
        Closed days
      </h3>
      {cal.closures.length === 0 ? <p className="hint">No upcoming closures.</p> : null}
      <div className="tech-list">
        {cal.closures.map((c) => (
          <div key={c.date} className="tech-row">
            <span>
              <strong>{c.date}</strong>
              {c.reason ? ` · ${c.reason}` : ""}
            </span>
            <button type="button" disabled={busy} onClick={() => reopen(c.date)}>
              <Icon name="undo" />
              Reopen
            </button>
          </div>
        ))}
      </div>
      <div className="inline-form">
        <label>
          <span>
            <Icon name="calendar" />
            Date
          </span>
          <input type="date" value={closureDate} min={dealer.today} onChange={(e) => setClosureDate(e.target.value)} />
        </label>
        <label>
          <span>
            <Icon name="info" />
            Reason
          </span>
          <input
            type="text"
            value={closureReason}
            maxLength={120}
            placeholder="e.g. Bank holiday"
            onChange={(e) => setClosureReason(e.target.value)}
          />
        </label>
        <button type="button" disabled={busy || !closureDate} onClick={addClosure}>
          <Icon name="lock" />
          Close that day
        </button>
      </div>
    </div>
  );
}
