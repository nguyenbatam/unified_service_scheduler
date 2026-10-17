import { useEffect, useState } from "react";
import { AdminNotice } from "./AdminNotice";
import { confirmAction } from "./ConfirmDialog";
import { ErrorBanner } from "./ErrorBanner";
import { MAX_PAGE_LIMIT, api } from "./api";
import { aggregateLabel, eventLabel } from "./labels";
import type { FailedOutboxEvent } from "./types";
import { useAdminAction } from "./useAdminAction";
import { usePagedList } from "./usePagedList";
import { Icon } from "./Icon";
import { EmptyState } from "./Illustration";

/** Failed events per page ("Show more" for the rest). */
const OUTBOX_PAGE = 50;

/**
 * Group admin: events the outbox worker gave up on (Kafka kept rejecting their content). They are
 * lost to live boards and notifications until requeued, so fix the cause first, then requeue.
 */
export function OutboxPanel() {
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [reloads, setReloads] = useState(0);
  const { busy, error: actionError, notice, setNotice, save } = useAdminAction();
  const list = usePagedList<FailedOutboxEvent>(
    (cursor, limit) => api.failedOutbox({ limit, cursor }),
    "outbox-failed",
    { pageSize: OUTBOX_PAGE, maxPageSize: MAX_PAGE_LIMIT, reloadKey: reloads },
  );
  const events = list.items;
  const error = actionError || list.error;
  const load = () => setReloads((n) => n + 1);

  // Only rows still listed stay picked (a requeued one leaves the list).
  useEffect(() => {
    if (events) setPicked((prev) => new Set(events.filter((e) => prev.has(e.id)).map((e) => e.id)));
  }, [events]);

  function toggle(id: string, on: boolean) {
    setPicked((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  async function requeue() {
    const ids = [...picked];
    if (ids.length === 0) return;
    if (!(await confirmAction(`Send ${ids.length} failed event(s) to Kafka again? Do this once the cause is fixed.`))) return;
    let message = "";
    const ok = await save(async () => {
      const res = await api.requeueOutbox(ids);
      message =
        `${res.requeued} event(s) requeued.` +
        (res.skipped.length ? ` ${res.skipped.length} skipped: their appointment has an event in flight — try again shortly.` : "");
      load();
    });
    if (ok) setNotice(message);
  }

  return (
    <div>
      <h3>
        <Icon name="alert" />
        Failed events
      </h3>
      <p className="hint">
        Events the outbox stopped retrying after Kafka rejected their content. Live boards and customer
        notifications miss them until they are requeued.
      </p>
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />
      {events === null ? (
        error ? null : <p className="hint">Loading…</p>
      ) : events.length === 0 ? (
        <EmptyState art="allClear">No failed events — everything was delivered.</EmptyState>
      ) : (
        <>
          <table className="roster-table">
            <thead>
              <tr>
                <th>
                  <input
                    type="checkbox"
                    aria-label="Select all"
                    checked={picked.size === events.length}
                    onChange={(e) => setPicked(e.target.checked ? new Set(events.map((x) => x.id)) : new Set())}
                  />
                </th>
                <th>Event</th>
                <th>Aggregate</th>
                <th>Tries</th>
                <th>Last error</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id}>
                  <td>
                    <input
                      type="checkbox"
                      aria-label={`Select ${eventLabel(e.eventType)} #${e.seq}`}
                      checked={picked.has(e.id)}
                      onChange={(x) => toggle(e.id, x.target.checked)}
                    />
                  </td>
                  <td>
                    {eventLabel(e.eventType)} <small className="hint">#{e.seq}</small>
                  </td>
                  <td>
                    {aggregateLabel(e.aggregateType)} <small className="hint">{e.aggregateId.slice(0, 8)}</small>
                  </td>
                  <td>
                    {e.attempts} ({e.rejections} rejected)
                  </td>
                  <td>{e.lastError}</td>
                  <td>{new Date(e.createdAt).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="actions">
            {list.hasMore ? (
              <>
                <button type="button" disabled={list.loading} onClick={() => void list.more()}>
                  <Icon name="chevronDown" />
                  {list.loading ? "Loading…" : "Show more"}
                </button>{" "}
              </>
            ) : null}
            <button type="button" className="primary" disabled={busy || picked.size === 0} onClick={requeue}>
              <Icon name="refresh" />
              Requeue {picked.size || ""} selected
            </button>{" "}
            <button type="button" disabled={busy} onClick={load}>
              <Icon name="refresh" />
              Refresh
            </button>
          </div>
        </>
      )}
    </div>
  );
}
