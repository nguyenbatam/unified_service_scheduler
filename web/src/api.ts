import {
  authHeaders,
  currentSession,
  refreshedSession,
  signalAuthExpired,
  signOutPending,
  withRefreshLock,
  type AuthSession,
} from "./auth";
import type {
  Appointment,
  AppointmentChange,
  AppointmentPage,
  AppointmentPageQuery,
  AuthResponse,
  Availability,
  Closure,
  Customer,
  CustomerCreate,
  CustomerQuery,
  Dealership,
  DealershipCalendar,
  DealershipPolicy,
  FailedOutboxEvent,
  NextAvailability,
  BayType,
  Page,
  PageRequest,
  Schedule,
  ServiceBay,
  ServiceType,
  TechnicianAdmin,
  TechnicianRoster,
  TechnicianShift,
  TechnicianSkill,
  Vehicle,
  VehicleQuery,
  WeekdayHours,
} from "./types";
import { vehicleParams } from "./vehicles";

/** retryAfter is the server's Retry-After in seconds, when it sent one (429, 503). */
export type ApiError = Error & { code: string; status: number; retryAfter?: number };

export function isApiError(err: unknown): err is ApiError {
  return err instanceof Error && "status" in err;
}

/**
 * NetworkError is a request that got no HTTP answer (offline, DNS, connection reset): fetch, or
 * reading the body, rejected. Only this — not any TypeError a bug might throw — reads as "can't
 * reach the server". An abort (AbortSignal) is not wrapped.
 */
export class NetworkError extends Error {
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause), { cause });
    this.name = "NetworkError";
  }
}

function isAbort(err: unknown): boolean {
  // A DOMException is not an Error in every runtime: check the name only.
  return typeof err === "object" && err !== null && (err as { name?: unknown }).name === "AbortError";
}

/** Runs one transport step, turning its rejection into a NetworkError (aborts pass through). */
async function transport<T>(step: () => Promise<T>): Promise<T> {
  try {
    return await step();
  } catch (err) {
    throw isAbort(err) ? err : new NetworkError(err);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  return (await requestWithHeaders<T>(path, init)).body;
}

/** The custom header the session endpoints (refresh, logout by cookie) require against CSRF. */
const CSRF_HEADER = { "X-Requested-With": "fetch" } as const;
const REFRESH_PATH = "/api/v1/auth/refresh";
/** A 401 from these is the answer itself (wrong password, dead cookie): never refreshed and retried. */
const SESSION_PATHS = ["/api/v1/auth/login", "/api/v1/auth/register", REFRESH_PATH, "/api/v1/auth/logout"];

function send(path: string, init: RequestInit | undefined, token: string | null): Promise<Response> {
  const headers = new Headers(authHeaders(init?.headers, token));
  // Same origin: the refresh cookie (Path=/api/v1/auth) rides along where it applies, and a
  // login's Set-Cookie is stored.
  return transport(() => fetch(path, { ...init, headers, credentials: "same-origin" }));
}

/** Reads a response: its JSON body, or the ApiError it stands for. */
async function readBody<T>(res: Response): Promise<{ body: T; headers: Headers }> {
  return parseBody<T>(res, await transport(() => res.text()));
}

/** A session request (refresh, logout) that hangs fails after this, so it can be retried at once. */
export const SESSION_REQUEST_TIMEOUT_MS = 10_000;

/**
 * Sends a session request and reads its body within SESSION_REQUEST_TIMEOUT_MS. A network
 * failure or the timeout rejects with NetworkError; an abort of `outer` (the caller gave up)
 * rejects with that abort.
 */
async function sendTimed(
  path: string,
  init: RequestInit,
  token: string | null,
  outer?: AbortSignal,
): Promise<{ res: Response; text: string }> {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), SESSION_REQUEST_TIMEOUT_MS);
  const forward = () => ctl.abort();
  outer?.addEventListener("abort", forward);
  try {
    const res = await fetch(path, {
      ...init,
      headers: new Headers(authHeaders(init.headers, token)),
      credentials: "same-origin",
      signal: ctl.signal,
    });
    return { res, text: await res.text() };
  } catch (err) {
    if (outer?.aborted) throw err;
    throw new NetworkError(ctl.signal.aborted ? new Error("the server did not answer in time") : err);
  } finally {
    clearTimeout(timer);
    outer?.removeEventListener("abort", forward);
  }
}

function parseBody<T>(res: Response, text: string): { body: T; headers: Headers } {
  let body: unknown = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch (err) {
    // A gateway error page (e.g. an Nginx 429/502) is not JSON; report the status instead.
    if (res.ok) throw err;
  }
  if (!res.ok) {
    const apiErr = (body as { error?: { code?: string; message?: string } } | null)?.error;
    const code = apiErr?.code ?? "http_error";
    // busy = database lock/statement timeout or the request deadline (503, Retry-After: 1). A write
    // cut off by the deadline may still have been saved: retries replay (Idempotency-Key, If-Match).
    const message =
      code === "busy"
        ? "The scheduler is busy right now — your change may not have been saved. Please refresh and retry in a moment."
        : (apiErr?.message ?? (res.statusText || `HTTP ${res.status}`));
    const retryAfter = Number(res.headers.get("Retry-After"));
    throw Object.assign(new Error(message), {
      code,
      status: res.status,
      ...(retryAfter > 0 ? { retryAfter } : {}),
    }) as ApiError;
  }
  return { body: body as T, headers: res.headers };
}

/**
 * The session to retry a request with after its token `sent` got a 401 under session `started`:
 * the token this tab refreshed to since, or a fresh one (one refresh shared by every caller).
 * Null — no retry — when the tab signed out or switched accounts meanwhile (a request made for
 * one account is never replayed as another), or the refresh failed: refused (that already
 * signed out, refreshSession) or transient (the session stays for the next attempt).
 */
async function sessionAfter401(started: AuthSession, sent: string): Promise<AuthSession | null> {
  const now = currentSession();
  if (!now || now.key !== started.key) return null;
  if (now.token !== sent) return now;
  let next: AuthSession | null;
  try {
    next = await refreshSession();
  } catch {
    return null;
  }
  return next && next.key === started.key ? next : null;
}

async function requestWithHeaders<T>(
  path: string,
  init?: RequestInit,
): Promise<{ body: T; headers: Headers }> {
  // The session this request is made for: a 401 refreshes and retries it once, or ends it (only it).
  const started = currentSession();
  const token = started?.token ?? null;
  let res = await send(path, init, token);
  if (res.status === 401 && started && token && !SESSION_PATHS.some((p) => path.startsWith(p))) {
    // Every body we send is a string (JSON), so the request can go again as is; writes carry
    // Idempotency-Key / If-Match, so a replay never applies twice.
    const retry = await sessionAfter401(started, token);
    if (retry) {
      res = await send(path, init, retry.token);
      // Refused again with a token just issued: the session is gone (revoked, user disabled).
      if (res.status === 401) signalAuthExpired(started.key);
    }
  }
  return readBody<T>(res);
}

let refreshing: Promise<AuthSession | null> | null = null;

/**
 * Exchanges the HttpOnly refresh cookie for a new access token (POST /auth/refresh, rotating the
 * cookie). Concurrent callers share one request; across tabs the Web Lock lets one tab at a time
 * spend the cookie, and a tab that waited takes the session the winner broadcast instead of
 * refreshing again (the server's short reuse grace covers browsers without Web Locks). Resolves
 * to the session (also adopted and broadcast), or null when there is none: the cookie is missing,
 * expired, or revoked (401/403) — which ends this tab's session — or the tab signed out
 * meanwhile. Rejects on a transient failure (network or timeout, 429, 5xx — after one immediate
 * retry for network/timeout/5xx): the session stays.
 */
export function refreshSession(): Promise<AuthSession | null> {
  // An unconfirmed sign-out: the cookie must not bring the session back.
  if (signOutPending()) return Promise.resolve(null);
  if (refreshing) return refreshing;
  const before = currentSession();
  refreshing = withRefreshLock(async () => {
    const now = currentSession();
    // Signed out or switched accounts meanwhile, or another tab refreshed (or, at boot, signed
    // in) while this one waited for the lock: nothing to spend the cookie on.
    if (before ? now?.key !== before.key : now !== null) return now;
    if (now && now.token !== before?.token) return now;
    const post = () => sendTimed(REFRESH_PATH, { method: "POST", headers: CSRF_HEADER }, null);
    // A refresh lost on the way back (the server may have rotated the cookie, its Set-Cookie
    // lost) goes again at once, well inside the server's reuse grace — a retry after it would
    // read as token reuse and revoke every session. Only then the caller's backoff applies.
    let answer = await post().catch(() => null);
    if (!answer || answer.res.status >= 500) answer = await post();
    const { res, text } = answer;
    if (res.status === 401 || res.status === 403) {
      if (before) signalAuthExpired(before.key);
      return null;
    }
    const { body } = parseBody<AuthResponse>(res, text);
    const after = currentSession();
    // Signed out (or switched) while the request was out: never bring the old session back.
    if (before ? after?.key !== before.key : after !== null) return after;
    return refreshedSession(body);
  }).finally(() => {
    refreshing = null;
  });
  return refreshing;
}

export function appointmentPageParams(q: AppointmentPageQuery): URLSearchParams {
  const params = new URLSearchParams();
  if (q.dealershipId) params.set("dealershipId", q.dealershipId);
  if (q.customerId) params.set("customerId", q.customerId);
  if (q.vehicleId) params.set("vehicleId", q.vehicleId);
  if (q.when) params.set("when", q.when);
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  return params;
}

function withQuery(path: string, params: URLSearchParams): string {
  const query = params.toString();
  return query ? `${path}?${query}` : path;
}

/**
 * fetchPage reads one page of a list endpoint (every list is paged): `params` are its filters, to
 * which the page's `limit` and `cursor` are added; the next page's token comes from X-Next-Cursor.
 */
export async function fetchPage<T>(
  path: string,
  params: URLSearchParams = new URLSearchParams(),
  page: PageRequest = {},
  init?: RequestInit,
): Promise<Page<T>> {
  const q = new URLSearchParams(params);
  if (page.limit) q.set("limit", String(page.limit));
  if (page.cursor) q.set("cursor", page.cursor);
  const { body, headers } = await requestWithHeaders<T[]>(withQuery(path, q), init);
  return { items: body ?? [], nextCursor: headers.get("X-Next-Cursor") || null };
}

const defaultID = (item: unknown) => String((item as { id?: unknown }).id);

/** Appends page to items, leaving out rows already shown (one edited between pages may come again). */
export function appendPage<T>(items: T[], page: T[], idOf: (item: T) => string = defaultID): T[] {
  const seen = new Set(items.map(idOf));
  return [...items, ...page.filter((item) => !seen.has(idOf(item)))];
}

/** The API's maximum `limit` of most lists (appointments, history, catalog, technicians, failed events). */
export const MAX_PAGE_LIMIT = 200;
/** The maximum `limit` of GET /customers. */
export const MAX_CUSTOMER_LIMIT = 100;
/** Page size fetchAll asks for (the API maximum of these lists). */
export const FETCH_ALL_PAGE = MAX_PAGE_LIMIT;
/** fetchAll stops after this many items: it is meant for dropdowns, never for lists that grow. */
export const FETCH_ALL_MAX = 2000;

/** Every row of a small list, or its first FETCH_ALL_MAX when `truncated` (more exist). */
export type AllRows<T> = { items: T[]; truncated: boolean };

/**
 * fetchAll reads every page of a small list (the catalog, a dealership's technicians) for a
 * dropdown, following X-Next-Cursor (a row edited between pages may come twice: it is kept once);
 * it stops at FETCH_ALL_MAX items and says so (`truncated`), so the dropdown can tell the user.
 * Lists that can grow (customers, vehicles, history, failed events) page with "Show more" instead.
 */
export async function fetchAll<T extends { id: string }>(
  path: string,
  params?: URLSearchParams,
  init?: RequestInit,
): Promise<AllRows<T>> {
  let items: T[] = [];
  let cursor: string | null = null;
  do {
    const page: Page<T> = await fetchPage<T>(path, params, { limit: FETCH_ALL_PAGE, cursor }, init);
    items = appendPage(items, page.items);
    cursor = page.nextCursor;
  } while (cursor && items.length < FETCH_ALL_MAX);
  return { items: items.slice(0, FETCH_ALL_MAX), truncated: cursor !== null || items.length > FETCH_ALL_MAX };
}

/** If-Match header for optimistic concurrency; the server answers 412 when version is stale. */
function ifMatch(version?: number): HeadersInit {
  return version ? { "If-Match": `"${version}"` } : {};
}

function jsonRequest<T>(
  path: string,
  method: string,
  body?: unknown,
  extraHeaders?: HeadersInit,
): Promise<T> {
  const headers = new Headers(extraHeaders);
  headers.set("Content-Type", "application/json");
  return request<T>(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

function appointmentAction(id: string, action: string, version?: number, body?: unknown) {
  return jsonRequest<Appointment>(
    `/api/v1/appointments/${id}/${action}`,
    "POST",
    body,
    ifMatch(version),
  );
}

/** One job line goes as serviceTypeId, several as serviceTypeIds (order kept); never both. */
function serviceParams(ids: string[]): { serviceTypeId: string } | { serviceTypeIds: string[] } {
  return ids.length === 1 ? { serviceTypeId: ids[0] } : { serviceTypeIds: ids };
}

export type AvailabilityQuery = {
  dealershipId: string;
  vehicleId: string;
  /** 1–5 job lines. */
  serviceTypeIds: string[];
  isWaiter?: boolean;
  /** Reschedule view: ignore this appointment's own hold (same dealership and vehicle). */
  excludeAppointmentId?: string;
};

export function availabilityParams(q: AvailabilityQuery): URLSearchParams {
  const params = new URLSearchParams({ dealershipId: q.dealershipId, vehicleId: q.vehicleId });
  if (q.serviceTypeIds.length === 1) params.set("serviceTypeId", q.serviceTypeIds[0]);
  else params.set("serviceTypeIds", q.serviceTypeIds.join(","));
  if (q.isWaiter) params.set("isWaiter", "true");
  if (q.excludeAppointmentId) params.set("excludeAppointmentId", q.excludeAppointmentId);
  return params;
}

export type DealershipPatch = {
  openTime?: string;
  closeTime?: string;
  lunchStart?: string;
  lunchEnd?: string;
  /** 0–100; 0 = unlimited. */
  stagingCapacity?: number;
  stagingWindowMinutes?: 15 | 30 | 60;
  policy?: Partial<DealershipPolicy>;
};

/** A dealership to open (group admin). Omitted policy fields take the server defaults. */
export type NewDealership = {
  name: string;
  /** IANA zone, e.g. "Europe/London". */
  timezone: string;
  openTime: string;
  closeTime: string;
  /** HH:MM with lunchEnd, or both omitted. */
  lunchStart?: string;
  lunchEnd?: string;
  stagingCapacity?: number;
  stagingWindowMinutes?: 15 | 30 | 60;
  policy?: Partial<DealershipPolicy>;
};

export type NewBay = { name: string; bayType: BayType };

export type NewTechnician = {
  dealershipId: string;
  name: string;
  /** 1–4. */
  skillLevel: number;
  skills: TechnicianSkill[];
};

/** Vehicles are not part of the catalog: they are searched a page at a time (useVehicleList). */
export type Catalog = {
  dealerships: Dealership[];
  services: ServiceType[];
  /** A list stopped at FETCH_ALL_MAX: its dropdowns say so. */
  truncated: { dealerships: boolean; services: boolean };
};

/**
 * fresh asks the server to skip its short per-instance catalog cache (Cache-Control: no-cache):
 * use it after an edit or a CONFIG_UPDATED event, which another instance may not have seen yet.
 */
export async function fetchCatalog(opts: { fresh?: boolean } = {}): Promise<Catalog> {
  const init: RequestInit | undefined = opts.fresh ? { headers: { "Cache-Control": "no-cache" } } : undefined;
  const [dealerships, services] = await Promise.all([api.dealerships(init), api.serviceTypes(init)]);
  return {
    dealerships: dealerships.items,
    services: services.items,
    truncated: { dealerships: dealerships.truncated, services: services.truncated },
  };
}

export type AppointmentAction = (appt: Appointment) => Promise<Appointment>;

export const api = {
  login: (payload: { email: string; password: string }) =>
    jsonRequest<AuthResponse>("/api/v1/auth/login", "POST", payload),
  register: (payload: { email: string; password: string; name: string }) =>
    jsonRequest<AuthResponse>("/api/v1/auth/register", "POST", payload),
  me: () => request<AuthResponse["user"]>("/api/v1/auth/me"),
  /**
   * Signs out every session of this user (all tabs and devices): sent with `token` (Bearer, when
   * this tab still has it) and the refresh cookie (either is enough for the server). "confirmed"
   * on 204, or 401 (nothing left to revoke); "failed" otherwise (network, timeout, 429, 5xx):
   * the session may still be live, retry.
   */
  logout: async (token: string | null, signal?: AbortSignal): Promise<"confirmed" | "failed"> => {
    try {
      const { res, text } = await sendTimed("/api/v1/auth/logout", { method: "POST", headers: CSRF_HEADER }, token, signal);
      parseBody<void>(res, text);
      return "confirmed";
    } catch (err) {
      return isApiError(err) && err.status === 401 ? "confirmed" : "failed";
    }
  },
  wsTicket: () =>
    jsonRequest<{ ticket: string; expiresAt: string }>("/api/v1/auth/ws-ticket", "POST"),
  /** Every dealership (all pages: a dropdown). */
  dealerships: (init?: RequestInit) => fetchAll<Dealership>("/api/v1/dealerships", undefined, init),
  /** The whole service catalog (all pages: a dropdown). */
  serviceTypes: (init?: RequestInit) => fetchAll<ServiceType>("/api/v1/service-types", undefined, init),
  /** One page (default 50, max 200), optionally narrowed by `q`; follow `nextCursor` for more. */
  vehicles: (q: VehicleQuery = {}) => fetchPage<Vehicle>("/api/v1/vehicles", vehicleParams(q)),
  /** 404 for another customer's vehicle. */
  vehicle: (id: string) => request<Vehicle>(`/api/v1/vehicles/${id}`),
  /** Staff: one page of customers whose name, phone or email contains `q` (default 20, max 100). */
  customers: ({ q, ...page }: CustomerQuery = {}) => {
    const params = new URLSearchParams();
    if (q?.trim()) params.set("q", q.trim());
    return fetchPage<Customer>("/api/v1/customers", params, page);
  },
  /** Staff: record a walk-in customer (no account). */
  createCustomer: (payload: CustomerCreate) => jsonRequest<Customer>("/api/v1/customers", "POST", payload),
  /** customerId: staff only (required for staff, who register a walk-in customer's car). */
  createVehicle: (payload: { vin: string; make: string; model: string; year: number; customerId?: string }) =>
    jsonRequest<Vehicle>("/api/v1/vehicles", "POST", payload),
  availability: (q: AvailabilityQuery & { date: string }) => {
    const params = availabilityParams(q);
    params.set("date", q.date);
    return request<Availability>(`/api/v1/availability?${params}`);
  },
  /**
   * Earliest free slots from `from` (YYYY-MM-DD) over up to `days` days, at most `perDay` from one
   * day, in time order. `prefer: "recommended"` spends each day's share on its recommended starts first.
   */
  nextAvailability: (
    q: AvailabilityQuery & { from?: string; days?: number; limit?: number; perDay?: number; prefer?: "recommended" },
  ) => {
    const params = availabilityParams(q);
    if (q.from) params.set("from", q.from);
    if (q.days) params.set("days", String(q.days));
    if (q.limit) params.set("limit", String(q.limit));
    if (q.perDay) params.set("perDay", String(q.perDay));
    if (q.prefer) params.set("prefer", q.prefer);
    return request<NextAvailability>(`/api/v1/availability/next?${params}`);
  },
  schedule: (dealershipId: string, date: string) => {
    const params = new URLSearchParams({ dealershipId, date });
    return request<Schedule>(`/api/v1/schedule?${params}`);
  },
  /** One page of `when`'s listing (upcoming soonest first, else newest first); follow `nextCursor`. */
  appointmentPage: (q: AppointmentPageQuery = {}): Promise<AppointmentPage> =>
    fetchPage<Appointment>("/api/v1/appointments", appointmentPageParams(q)),
  appointment: (id: string) => request<Appointment>(`/api/v1/appointments/${id}`),
  book: (
    { serviceTypeIds, ...rest }: {
      dealershipId: string;
      vehicleId: string;
      /** 1–5 job lines, order kept. */
      serviceTypeIds: string[];
      startAt: string;
      isWaiter?: boolean;
    },
    idempotencyKey: string,
  ) =>
    jsonRequest<Appointment>(
      "/api/v1/appointments",
      "POST",
      { ...rest, ...serviceParams(serviceTypeIds) },
      { "Idempotency-Key": idempotencyKey },
    ),
  cancel: (a: Appointment) => appointmentAction(a.id, "cancel", a.version),
  checkIn: (a: Appointment) => appointmentAction(a.id, "check-in", a.version),
  start: (a: Appointment) => appointmentAction(a.id, "start", a.version),
  complete: (a: Appointment) => appointmentAction(a.id, "complete", a.version),
  noShow: (a: Appointment) => appointmentAction(a.id, "no-show", a.version),
  /** Undo a no-show for a late arrival (back to checked in) while the booked window lasts. */
  reinstate: (a: Appointment) => appointmentAction(a.id, "reinstate", a.version),
  extend: (a: Appointment, minutes: number) =>
    appointmentAction(a.id, "extend", a.version, { minutes }),
  reschedule: (a: Appointment, startAt: string, isWaiter?: boolean) =>
    appointmentAction(a.id, "reschedule", a.version, { startAt, isWaiter }),
  /** Staff: move to another technician and/or bay in the same window (409 busy, 422 unfit). */
  reassign: (a: Appointment, target: { technicianId?: string; serviceBayId?: string }) =>
    appointmentAction(a.id, "reassign", a.version, target),
  /** One page of the audit trail, oldest first; follow `nextCursor` for later changes. */
  history: (id: string, page: PageRequest = {}) =>
    fetchPage<AppointmentChange>(`/api/v1/appointments/${id}/history`, undefined, page),
  /** Every technician of the dealership (all pages: the admin's editor and dropdowns). */
  adminTechnicians: (dealershipId?: string) => {
    const params = new URLSearchParams();
    if (dealershipId) params.set("dealershipId", dealershipId);
    return fetchAll<TechnicianAdmin>("/api/v1/admin/technicians", params);
  },
  createDealership: (payload: NewDealership) =>
    jsonRequest<Dealership>("/api/v1/admin/dealerships", "POST", payload),
  createBay: (dealershipId: string, payload: NewBay) =>
    jsonRequest<ServiceBay>(`/api/v1/admin/dealerships/${dealershipId}/bays`, "POST", payload),
  createTechnician: (payload: NewTechnician) =>
    jsonRequest<TechnicianAdmin>("/api/v1/admin/technicians", "POST", payload),
  patchDealership: (id: string, version: number, payload: DealershipPatch) =>
    jsonRequest<Dealership>(`/api/v1/admin/dealerships/${id}`, "PATCH", payload, ifMatch(version)),
  calendar: (dealershipId: string) =>
    request<DealershipCalendar>(`/api/v1/admin/dealerships/${dealershipId}/calendar`),
  putWeeklyHours: (dealershipId: string, version: number, weekly: WeekdayHours[]) =>
    jsonRequest<DealershipCalendar>(
      `/api/v1/admin/dealerships/${dealershipId}/hours`,
      "PUT",
      { weekly },
      ifMatch(version),
    ),
  addClosure: (dealershipId: string, version: number, closure: Closure) =>
    jsonRequest<DealershipCalendar>(
      `/api/v1/admin/dealerships/${dealershipId}/closures`,
      "POST",
      closure,
      ifMatch(version),
    ),
  deleteClosure: (dealershipId: string, version: number, date: string) =>
    request<DealershipCalendar>(`/api/v1/admin/dealerships/${dealershipId}/closures/${date}`, {
      method: "DELETE",
      headers: ifMatch(version),
    }),
  patchServiceType: (
    id: string,
    version: number,
    payload: {
      durationMinutes?: number;
      minSkillLevel?: number;
      requiredSkill?: number;
      requiredBayType?: number;
    },
  ) =>
    jsonRequest<ServiceType>(
      `/api/v1/admin/service-types/${id}`,
      "PATCH",
      payload,
      ifMatch(version),
    ),
  /** Group admin: one page of failed (poisoned) outbox events, oldest first. */
  failedOutbox: (page: PageRequest = {}) =>
    fetchPage<FailedOutboxEvent>("/api/v1/admin/outbox/failed", undefined, page),
  /** Group admin: return failed outbox events to pending; `skipped` had an event in flight (retry). */
  requeueOutbox: (ids: string[]) =>
    jsonRequest<{ requeued: number; skipped: string[] }>("/api/v1/admin/outbox/requeue", "POST", { ids }),
  roster: (technicianId: string) =>
    request<TechnicianRoster>(`/api/v1/admin/technicians/${technicianId}/roster`),
  /** `[]` = the technician works the dealership's opening hours. */
  putShifts: (technicianId: string, version: number, shifts: TechnicianShift[]) =>
    jsonRequest<TechnicianRoster>(
      `/api/v1/admin/technicians/${technicianId}/shifts`,
      "PUT",
      { shifts },
      ifMatch(version),
    ),
  addAbsence: (
    technicianId: string,
    version: number,
    absence: { startsAt: string; endsAt: string; reason?: string },
  ) =>
    jsonRequest<TechnicianRoster>(
      `/api/v1/admin/technicians/${technicianId}/absences`,
      "POST",
      absence,
      ifMatch(version),
    ),
  deleteAbsence: (technicianId: string, version: number, absenceId: string) =>
    request<TechnicianRoster>(`/api/v1/admin/technicians/${technicianId}/absences/${absenceId}`, {
      method: "DELETE",
      headers: ifMatch(version),
    }),
  /** skills: the full set (1–3); removing one an upcoming booking needs is 409. */
  patchTechnician: (
    id: string,
    version: number,
    payload: { skillLevel?: number; active?: boolean; skills?: TechnicianSkill[] },
  ) =>
    jsonRequest<TechnicianAdmin>(
      `/api/v1/admin/technicians/${id}`,
      "PATCH",
      payload,
      ifMatch(version),
    ),
};
