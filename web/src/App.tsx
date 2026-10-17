import { useEffect, useMemo, useRef, useState } from "react";
import { AppointmentPanel } from "./AppointmentPanel";
import { AdminPanel } from "./AdminPanel";
import { AuthPanel } from "./AuthPanel";
import { BookingRequest } from "./BookingRequest";
import { ConfirmHost } from "./ConfirmDialog";
import { AppointmentList } from "./AppointmentList";
import { VehicleForm } from "./CustomerPanels";
import { DayBoard } from "./DayBoard";
import { ErrorBanner } from "./ErrorBanner";
import { CustomerLayout, StaffLayout } from "./Layouts";
import { SlotGrid } from "./SlotGrid";
import { StaffAppointments } from "./StaffAppointments";
import { StaffWalkIn } from "./StaffWalkIn";
import { Toast, useToast } from "./Toast";
import { WorkshopBar } from "./WorkshopBar";
import { isGroupAdmin } from "./admin";
import { api, fetchCatalog, isApiError, type Catalog } from "./api";
import { bookableDealerships, bookingReview, dealershipToday } from "./booking";
import { errorText } from "./errors";
import {
  adoptFetched,
  appointmentRefresh,
  availabilityRefresh,
  boardAffected,
  catalogAffected,
  coalesce,
  configRefresh,
  listsAffected,
  liveSubscription,
  noticeAffects,
  type BoardView,
  type BookingView,
  type ViewRefresh,
} from "./live";
import { useAppointmentActions } from "./useAppointmentActions";
import { useAsyncAction } from "./useAsyncAction";
import { useBookingForm } from "./useBookingForm";
import { useLiveEvents } from "./useLiveEvents";
import { useSchedule } from "./useSchedule";
import { sameAccess, useSession } from "./useSession";
import { useVehicleList } from "./useVehicleList";
import { Role, isStaffRole, roleName, type Appointment, type AuthUser, type Customer, type Dealership, type ServiceType } from "./types";
import { Icon, roleIcon } from "./Icon";

/**
 * Scrolls an element into view (no-op where the DOM lacks it, e.g. jsdom) and, for the appointment
 * panel, moves focus to its heading so keyboard and screen-reader users land on what was opened.
 */
function reveal(el: HTMLElement | null, focusHeading = false) {
  if (!el) return;
  if (typeof el.scrollIntoView === "function") el.scrollIntoView({ behavior: "smooth", block: "start" });
  if (focusHeading) el.querySelector<HTMLElement>("h2")?.focus({ preventScroll: true });
}

/** "14:05" in the browser's zone: when this browser's session ends. */
function localClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function App() {
  const [dealerships, setDealerships] = useState<Dealership[]>([]);
  const [services, setServices] = useState<ServiceType[]>([]);
  // A catalog list that stopped at FETCH_ALL_MAX: its dropdowns say so.
  const [catalogTruncated, setCatalogTruncated] = useState<Catalog["truncated"]>({ dealerships: false, services: false });
  // Bumped by live appointment events and our own writes: the appointment lists reload.
  const [listKey, setListKey] = useState(0);
  const [appointment, setAppointment] = useState<Appointment | null>(null);
  const [loading, setLoading] = useState(true);
  const [revealTarget, setReveal] = useState<{ target: "panel" | "grid"; tick: number } | null>(null);
  // The walk-in desk's customer: staff can list their bookings.
  const [deskCustomer, setDeskCustomer] = useState<Customer | null>(null);
  const { busy, writing, error, setError, run } = useAsyncAction();
  const toast = useToast();
  const {
    session,
    checking,
    notice: authNotice,
    dismissNotice,
    expiresAt,
    dismissExpiry,
    signIn,
    expire,
    logout,
    signOut,
    retrySignOut,
    setUser,
  } = useSession(() => {
    setAppointment(null);
    setDeskCustomer(null);
    // Nothing of this user's form (car, grid, picked slot, reschedule) may reach the next one.
    form.reset();
    setError("");
  });
  const isStaff = session ? isStaffRole(session.user.role) : false;
  // Staff of one dealership book and see the board of their own only (any other is 403).
  const bookable = useMemo(() => bookableDealerships(session?.user, dealerships), [session?.user, dealerships]);
  const form = useBookingForm({ run, dealerships: bookable, appointment, isStaff });
  const panelRef = useRef<HTMLElement>(null);
  const gridRef = useRef<HTMLElement>(null);

  // The board has its own day (moving it never drops the picked slot); a new dealership starts it
  // on that dealership's today.
  const [board, setBoard] = useState({ dealershipId: "", date: "" });
  if (board.dealershipId !== form.dealershipId || (!board.date && form.dealership)) {
    // Today by the clock (the catalog's `today` goes stale in a tab left open overnight).
    setBoard({ dealershipId: form.dealershipId, date: form.dealership ? dealershipToday(form.dealership) : "" });
  }
  const { schedule, error: scheduleError, refresh: refreshSchedule } = useSchedule(isStaff && !loading, form.dealershipId, board.date);
  // Vehicles come a page at a time (search for staff); the first page of a session seeds the pick.
  const vehicleList = useVehicleList({
    sessionKey: loading ? undefined : session?.key,
    isStaff,
    selectedId: form.vehicleId,
    // A customer with exactly one car gets it picked; staff (who book for anyone) and customers
    // with several cars choose one, so a booking never goes to a car nobody picked.
    onFirstPage: (items) => {
      if (!isStaff && items.length === 1) form.defaultVehicle(items[0].id);
    },
    onError: setError,
  });
  const timezoneOf = (id: string) => dealerships.find((d) => d.id === id)?.timezone ?? "UTC";
  const policyOf = (id: string) => dealerships.find((d) => d.id === id)?.policy;
  const actions = useAppointmentActions({
    appointment,
    setAppointment,
    form,
    run,
    toast: toast.show,
    refreshSchedule,
    refreshLists: () => setListKey((k) => k + 1),
    timezoneOf,
    revealPanel: () => setReveal({ target: "panel", tick: Date.now() }),
  });

  useEffect(() => {
    if (!revealTarget) return;
    if (revealTarget.target === "panel") reveal(panelRef.current, true);
    else reveal(gridRef.current);
  }, [revealTarget]);

  // A customer's first ready form opens on the earliest free starts (once per session), so it
  // never lands on a day that is all "too soon" or fully booked.
  const autoOpened = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!session || isStaff || loading || !form.ready || form.availability || form.rescheduleTarget) return;
    if (autoOpened.current === session.key) return;
    autoOpened.current = session.key;
    void form.openFirstAvailable();
  });

  function applyCatalog(catalog: Awaited<ReturnType<typeof fetchCatalog>>, seedFor?: AuthUser) {
    setDealerships(catalog.dealerships);
    setServices(catalog.services);
    setCatalogTruncated(catalog.truncated);
    if (!seedFor) return;
    // Staff with a home dealership start there; group-wide admins and customers get the first.
    // Customers start on the first service (shown ticked, and in the review next to Confirm);
    // staff pick the job themselves, so nothing is booked by default.
    const home = catalog.dealerships.find((d) => d.id === seedFor.dealershipId) ?? catalog.dealerships[0];
    form.reset();
    form.seed(home, isStaffRole(seedFor.role) ? undefined : catalog.services[0]?.id);
  }

  // Reloads follow an edit or a CONFIG_UPDATED event: bypass the server's catalog cache, which
  // the instance answering may not have invalidated yet.
  async function reloadCatalog() {
    applyCatalog(await fetchCatalog({ fresh: true }));
  }

  /**
   * The live socket was closed over the session (close 1008): re-read the user. Changed access
   * (role, home dealership) starts the form over from what the user may now see; a revoked token
   * answers 401, which signs out (api.ts).
   */
  async function reloadUser() {
    const before = latest.current.session;
    if (!before) return;
    try {
      const user = await api.me();
      // Signed out, or another account adopted, while the request was out.
      if (latest.current.session?.key !== before.key) return;
      if (sameAccess(before.user, user)) {
        setUser(user, before.key);
        return;
      }
      setUser(user, before.key);
      setAppointment(null);
      applyCatalog(await fetchCatalog({ fresh: true }), user);
      toast.show("Your access changed — the view was reloaded.");
    } catch {
      // A 401 already signed out; anything else leaves the session for the next reconnect.
    }
  }

  // What the booking form and the board show now, for deciding what a live notice makes stale.
  function bookingView(): BookingView {
    const f = latest.current.form;
    return { dealershipId: f.dealershipId, serviceTypeIds: f.serviceTypeIds, date: f.date, timezone: f.dealership?.timezone };
  }
  function boardView(): BoardView {
    const { board: b, schedule: sch, form: f } = latest.current;
    return {
      dealershipId: b.dealershipId,
      date: b.date,
      timezone: sch?.timezone ?? f.dealership?.timezone,
      appointmentIds: new Set(sch?.appointments.map((a) => a.id) ?? []),
    };
  }

  // Live events: coalesce bursts into one refetch per view; always read the latest closures.
  const latest = useRef({ refreshSchedule, form, reloadCatalog, reloadUser, appointment, session, board, schedule });
  latest.current = { refreshSchedule, form, reloadCatalog, reloadUser, appointment, session, board, schedule };
  const refreshers = useMemo(() => {
    // What the notices of the burst made stale: the day grid, the first-available list, or both.
    let grid = false;
    let next = false;
    // Silent: only re-reads a grid or list the user already loaded, keeps the picked slot while it
    // is still offered, and never reports an error.
    const availability = coalesce(() => {
      const opts = { silent: true, grid, next };
      grid = false;
      next = false;
      void latest.current.form.refreshAvailability(opts);
    }, 250);
    return {
      board: coalesce(() => void latest.current.refreshSchedule({ silent: true }), 250),
      availability,
      /** Re-reads what `scope` says a notice made stale in the booking view. */
      view(scope: ViewRefresh) {
        if (scope === "none") return;
        next = true;
        if (scope === "all") grid = true;
        availability.trigger();
      },
      lists: coalesce(() => setListKey((k) => k + 1), 250),
      catalog: coalesce(() => void latest.current.reloadCatalog().catch(() => undefined), 250),
    };
  }, []);
  useEffect(
    () => () => [refreshers.board, refreshers.availability, refreshers.lists, refreshers.catalog].forEach((r) => r.cancel()),
    [refreshers],
  );

  /** Re-reads the panel's appointment; shown only while the panel still shows it (not older, not another). */
  function reloadAppointment(id: string) {
    void api
      .appointment(id)
      .then((fetched) => setAppointment((shown) => adoptFetched(shown, fetched)))
      .catch(() => undefined);
  }

  // A group admin looking at another dealership's appointment listens to all dealerships while the
  // panel is open (the hub filters by one at a time), so the panel and the board both stay live.
  const subscription = liveSubscription(form.dealershipId, appointment?.dealershipId, isGroupAdmin(session?.user));
  const wsState = useLiveEvents(session?.key, subscription, {
    onAppointment: (type, notice) => {
      // Only a notice on the board's day (or about an appointment it shows) re-reads the board;
      // only one on the grid's day re-reads the grid (the first-available list spans days).
      if (boardAffected(notice, boardView())) refreshers.board.trigger();
      refreshers.view(appointmentRefresh(type, notice, bookingView()));
      const { form: f, appointment: shown, session: s } = latest.current;
      const staffScope = s && isStaffRole(s.user.role) ? [f.dealershipId, shown?.dealershipId] : null;
      if (listsAffected(notice, staffScope)) refreshers.lists.trigger();
      // Version check drops duplicates/out-of-order events (reschedules keep the id).
      if (noticeAffects(notice, latest.current.appointment)) reloadAppointment(notice.id);
    },
    onAvailability: (notice) => refreshers.view(availabilityRefresh(notice, bookingView())),
    onConfig: (notice) => {
      // Technician changes (roster, skills) are not in the catalog: no reload for those.
      if (catalogAffected(notice)) refreshers.catalog.trigger();
      // Hours, roster, or a picked service changed: what is bookable in view may have too.
      refreshers.view(configRefresh(notice, bookingView()));
    },
    onResync: () => {
      refreshers.board.trigger();
      refreshers.view("all");
      refreshers.lists.trigger();
      refreshers.catalog.trigger();
      const current = latest.current.appointment;
      if (current) reloadAppointment(current.id);
    },
    // Only for the session this tab still uses (api.ts already ended it, session-scoped).
    onUnauthorized: (key) => expire(key),
    onSessionChanged: () => void latest.current.reloadUser(),
  });

  useEffect(() => {
    if (!session) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    const key = session.key;
    setLoading(true);
    (async () => {
      try {
        const user = await api.me();
        if (cancelled) return;
        setUser(user, key);
        const catalog = await fetchCatalog();
        if (cancelled) return;
        applyCatalog(catalog, user);
      } catch (err) {
        if (cancelled) return;
        // Only a refused session signs out (api.ts, after one refresh); a busy server,
        // throttling, or a network error keeps the session so a reload can recover.
        if (!(isApiError(err) && err.status === 401)) setError(errorText(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // Once per session (key): a refreshed access token is the same session.
  }, [session?.key]);

  function viewAppointment(a: Appointment) {
    // Opening another appointment ends a reschedule in progress (the grid excluded the old one).
    if (form.rescheduleTarget && form.rescheduleTarget.id !== a.id) form.stopReschedule();
    setAppointment(a);
    setReveal({ target: "panel", tick: Date.now() });
  }

  function closeAppointment() {
    form.stopReschedule();
    setAppointment(null);
  }

  async function startReschedule() {
    if (!appointment) return;
    setReveal({ target: "grid", tick: Date.now() });
    await form.startReschedule(appointment, timezoneOf(appointment.dealershipId));
  }

  if (checking) {
    // The boot refresh is out: neither the app nor (flashing) the login form yet.
    return (
      <main className="page" aria-busy="true">
        <p role="status">Checking your session…</p>
      </main>
    );
  }

  if (!session) return <AuthPanel notice={authNotice} onAuthed={signIn} signOut={signOut} onRetrySignOut={retrySignOut} />;

  if (loading) {
    return (
      <main className="page">
        <p>Loading…</p>
      </main>
    );
  }

  const formVehicle = vehicleList.options.find((v) => v.id === form.vehicleId);
  const review = form.selectedSlot
    ? bookingReview({
        vehicle: formVehicle,
        serviceTypeIds: form.serviceTypeIds,
        services,
        dealership: form.dealership,
        start: form.selectedSlot.start,
        isWaiter: form.isWaiter,
        isStaff,
      })
    : "";

  const bookingFlow = (
    <>
      <BookingRequest
        form={form}
        dealerships={bookable}
        services={services}
        truncated={catalogTruncated}
        vehicleList={vehicleList}
        isStaff={isStaff}
        busy={busy}
        writing={writing}
      />
      <SlotGrid
        sectionRef={gridRef}
        availability={form.availability}
        selectedSlot={form.selectedSlot}
        busy={busy}
        loading={form.slotsLoading}
        showCapacity={isStaff}
        onSelect={form.selectSlot}
        onConfirm={() => void actions.confirm()}
        rescheduleTarget={form.rescheduleTarget}
        onReschedule={(slot) => void actions.rescheduleTo(slot)}
        onStopReschedule={form.stopReschedule}
        review={review}
      />
    </>
  );

  const appointmentPanel = appointment ? (
    <AppointmentPanel
      sectionRef={panelRef}
      appointment={appointment}
      timezone={timezoneOf(appointment.dealershipId)}
      busy={busy}
      isStaff={isStaff}
      policy={policyOf(appointment.dealershipId)}
      // Another dealership's appointment: the staff tools read that dealership's bays and technicians.
      schedule={appointment.dealershipId === form.dealershipId ? schedule : null}
      rescheduling={form.rescheduleTarget?.id === appointment.id}
      onAction={(fn, opts) => void actions.runAction(fn, opts)}
      onStartReschedule={() => void startReschedule()}
      onClose={closeAppointment}
      heading={isStaff ? "Appointment" : undefined}
    />
  ) : null;

  return (
    <main className="page">
      <ConfirmHost />
      <header className="hero">
        <div className="hero-row">
          <div>
            <p className="eyebrow">Keyloop Scenario A</p>
            <h1>
              <Icon name="wrench" />
              Unified Service Scheduler
            </h1>
          </div>
          <div className="user-chip">
            <span>
              <Icon name={roleIcon(session.user.role)} />
              {session.user.name} · {roleName(session.user.role)}
            </span>
            <span className={`ws-dot ws-${wsState}`} title={`Live updates: ${wsState}`}>
              {wsState === "open" ? "Live" : wsState === "connecting" ? "Connecting…" : "Offline"}
            </span>
            <button
              type="button"
              // Revokes server-side (every tab and device), then signs out here and in the other tabs.
              onClick={() => void logout()}
            >
              <Icon name="logOut" />
              Sign out
            </button>
          </div>
        </div>
        <p className="lede">
          Book a bay and a qualified technician for the full service duration. Availability is checked before you
          confirm, and changes made elsewhere show up live.
        </p>
      </header>

      <div className="notices">
        <ErrorBanner message={error} onDismiss={() => setError("")} />
        {authNotice ? (
          <div className="banner info" role="status">
            <span>
              <Icon name="info" />
              {authNotice}
            </span>{" "}
            <button type="button" className="banner-close" aria-label="Dismiss notice" onClick={dismissNotice}>
              <Icon name="x" />
            </button>
          </div>
        ) : null}
        {expiresAt ? (
          <div className="banner warn" role="status">
            <span>
              <Icon name="clock" />
              Your session ends at {localClock(expiresAt)} — finish what you are doing, then sign out and sign in again to
              keep working.
            </span>
            <button type="button" className="banner-close" aria-label="Dismiss session warning" onClick={dismissExpiry}>
              <Icon name="x" />
            </button>
          </div>
        ) : null}
        <Toast message={toast.message} onDismiss={toast.dismiss} />
      </div>

      {isStaff ? (
        <StaffLayout
          admin={
            session.user.role === Role.Admin ? (
              <AdminPanel
                dealerships={dealerships}
                services={services}
                truncated={catalogTruncated}
                user={session.user}
                // Returned, so an admin form awaits the new version before it says "saved" (and
                // a quick second save sends the current If-Match).
                onChanged={() => reloadCatalog().catch((err: unknown) => setError(errorText(err)))}
              />
            ) : undefined
          }
          bar={
            <WorkshopBar
              dealerships={bookable}
              dealershipId={form.dealershipId}
              onDealershipChange={form.changeDealership}
              dealershipLocked={form.rescheduleTarget !== null}
              truncated={catalogTruncated.dealerships}
              date={board.date}
              onDateChange={(date) => setBoard((b) => ({ ...b, date }))}
            />
          }
          board={
            <DayBoard
              schedule={schedule}
              error={scheduleError}
              onRetry={() => void refreshSchedule()}
              date={board.date}
              selectedId={appointment?.id}
              onSelect={viewAppointment}
            />
          }
          appointment={appointmentPanel}
          booking={
            <>
              {bookingFlow}
              <StaffAppointments
                dealershipId={form.dealershipId}
                dealershipName={form.dealership?.name}
                vehicle={formVehicle ? { id: formVehicle.id, label: `${formVehicle.make} ${formVehicle.model} (${formVehicle.customerName})` } : undefined}
                customer={deskCustomer}
                timezoneOf={timezoneOf}
                selectedId={appointment?.id}
                onSelect={viewAppointment}
                reloadKey={listKey}
              />
              <details className="panel collapsible">
                <summary>
                  <Icon name="userPlus" />
                  Walk-in: find or add the customer, then their car
                </summary>
                <StaffWalkIn
                  onCustomer={setDeskCustomer}
                  onCreated={(v) => {
                    vehicleList.add(v);
                    form.changeVehicle(v.id);
                    toast.show(`Added ${v.make} ${v.model} for ${v.customerName}.`);
                  }}
                />
              </details>
            </>
          }
        />
      ) : (
        <CustomerLayout
          hasVehicles={!vehicleList.loaded || vehicleList.options.length > 0}
          mine={
            <AppointmentList
              title="My appointments"
              timezoneOf={timezoneOf}
              selectedId={appointment?.id}
              onSelect={viewAppointment}
              reloadKey={listKey}
              emptyText="No appointments yet — book your first one below."
            />
          }
          booking={
            <>
              {bookingFlow}
              {appointmentPanel}
            </>
          }
          vehicleForm={
            <VehicleForm
              first={vehicleList.loaded && vehicleList.options.length === 0}
              onCreated={(v) => {
                vehicleList.add(v);
                form.changeVehicle(v.id);
                toast.show(`Added ${v.make} ${v.model}.`);
              }}
            />
          }
        />
      )}
    </main>
  );
}
