import type { ReactNode } from "react";
import { ErrorBanner } from "./ErrorBanner";
import { MAX_PAGE_LIMIT, api } from "./api";
import { statusLabel } from "./labels";
import { formatClockRange, formatDate, formatDayLabel, zoneAbbr } from "./time";
import { statusName, type Appointment, type AppointmentPageQuery, type AppointmentWhen } from "./types";
import { type PagedList, usePagedList } from "./usePagedList";
import { Icon, statusIcon } from "./Icon";
import { EmptyState } from "./Illustration";

const PAGE_SIZE = 20;

type Props = {
  title: string;
  /** Filters (staff: dealership, customer or vehicle; customers always get only their own). */
  query?: Omit<AppointmentPageQuery, "cursor" | "limit" | "when">;
  timezoneOf: (dealershipId: string) => string;
  selectedId?: string;
  onSelect: (item: Appointment) => void;
  /** Change it (e.g. a live event counter) to reload both listings from their first page. */
  reloadKey?: unknown;
  /** Shown instead of the default empty text. */
  emptyText?: string;
  /** Controls under the title (e.g. the staff's filter). */
  toolbar?: ReactNode;
  /** Staff: each row names the customer and dealership too. */
  showOwner?: boolean;
};

type RowsProps = Pick<Props, "timezoneOf" | "selectedId" | "onSelect" | "showOwner"> & { items: Appointment[] };

function Rows({ items, timezoneOf, selectedId, onSelect, showOwner }: RowsProps) {
  return (
    <div className="tech-list">
      {items.map((a) => {
        const tz = timezoneOf(a.dealershipId);
        return (
          <button
            key={a.id}
            type="button"
            className={`tech-row ${a.id === selectedId ? "selected" : ""}`}
            aria-pressed={a.id === selectedId}
            onClick={() => onSelect(a)}
          >
            <span>
              <strong>{formatDayLabel(formatDate(a.startAt, tz))}</strong> {formatClockRange(a.startAt, a.endAt, tz)}{" "}
              {zoneAbbr(tz, a.startAt)} · {a.serviceName} · {a.vehicleLabel}
              {showOwner ? ` · ${a.customerName}` : ""} · {a.dealershipName}
            </span>
            <span className={`status-badge status-${statusName(a.status)}`}>
              <Icon name={statusIcon(a.status)} />
              {statusLabel(a.status)}
            </span>
          </button>
        );
      })}
    </div>
  );
}

type SectionProps = Pick<Props, "query" | "reloadKey"> & { when: AppointmentWhen };

/** One listing of the server (upcoming or past), a page at a time. */
function useSection({ query, reloadKey, when }: SectionProps) {
  const filterKey = JSON.stringify(query ?? {});
  return usePagedList<Appointment>(
    (cursor, limit) => api.appointmentPage({ ...query, when, limit, cursor }),
    `${when}|${filterKey}`,
    { pageSize: PAGE_SIZE, maxPageSize: MAX_PAGE_LIMIT, reloadKey },
  );
}

function MoreButton({ list, label }: { list: PagedList<Appointment>; label: string }) {
  if (!list.hasMore) return null;
  return (
    <div className="actions">
      <button type="button" disabled={list.loading} onClick={() => void list.more()}>
        <Icon name="chevronDown" />
        {list.loading ? "Loading…" : label}
      </button>
    </div>
  );
}

/**
 * Appointments in two listings read from the server separately, each with its own "Show more":
 * upcoming (booked and not yet ended, or on site — checked in or in progress — however late;
 * soonest first, so today's work is always on the first page) and past or cancelled (newest first).
 */
export function AppointmentList({ title, query, timezoneOf, selectedId, onSelect, reloadKey, emptyText, toolbar, showOwner = false }: Props) {
  const upcoming = useSection({ query, reloadKey, when: "upcoming" });
  const past = useSection({ query, reloadKey, when: "past" });
  const rows = { timezoneOf, selectedId, onSelect, showOwner };
  const error = upcoming.error || past.error;
  const loaded = upcoming.items !== null && past.items !== null;
  const empty = loaded && upcoming.items?.length === 0 && past.items?.length === 0;
  const pastItems = past.items ?? [];

  return (
    <section className="panel">
      <h2>
        <Icon name="list" />
        {title}
      </h2>
      {toolbar}
      <ErrorBanner message={error} />
      {!loaded ? (
        error ? null : <p className="hint">Loading appointments…</p>
      ) : empty ? (
        <EmptyState art="calendar">{emptyText ?? "No appointments yet."}</EmptyState>
      ) : (
        <>
          <h3>
            <Icon name="calendar" />
            Upcoming
          </h3>
          {upcoming.items?.length ? <Rows items={upcoming.items} {...rows} /> : <p className="hint">Nothing coming up.</p>}
          <MoreButton list={upcoming} label="Show later" />
          {pastItems.length > 0 ? (
            <details className="past-list">
              <summary>
                <Icon name="history" />
                Past and cancelled ({pastItems.length}
                {past.hasMore ? "+" : ""})
              </summary>
              <Rows items={pastItems} {...rows} />
              <MoreButton list={past} label="Show older" />
            </details>
          ) : null}
        </>
      )}
    </section>
  );
}
