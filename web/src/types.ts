export const Role = {
  Admin: 1,
  Dispatcher: 2,
  Customer: 3,
} as const;

export type UserRole = (typeof Role)[keyof typeof Role];

export type AuthUser = {
  id: string;
  email: string;
  name: string;
  role: UserRole;
  /** Present for customers only. */
  customerId?: string;
  /** Staff home dealership; absent = group-wide admin. */
  dealershipId?: string;
};

export function isStaffRole(role: UserRole): boolean {
  return role === Role.Admin || role === Role.Dispatcher;
}

/** POST /auth/login, /auth/register, /auth/refresh (the refresh cookie comes as Set-Cookie). */
export type AuthResponse = {
  /** Short-lived access token (Bearer); kept in memory only. */
  token: string;
  /** RFC 3339: when the access token expires. */
  expiresAt: string;
  /** RFC 3339: the sign-in's absolute end (refreshes never extend past it). */
  sessionExpiresAt: string;
  user: AuthUser;
};

/** SMALLINT codes — identical to Postgres / Go domain. */
export const Status = {
  Confirmed: 1,
  CheckedIn: 2,
  InProgress: 3,
  Completed: 4,
  Cancelled: 5,
  NoShow: 6,
} as const;

export const Bay = {
  Express: 1,
  General: 2,
  Diagnostic: 3,
  Alignment: 4,
} as const;

export const Skill = {
  Lubrication: 1,
  Brakes: 2,
  General: 3,
} as const;

/** A technician's grade and a service's minimum (ordered: a technician qualifies at or above it). */
export const SkillLevel = {
  Apprentice: 1,
  Technician: 2,
  Senior: 3,
  Master: 4,
} as const;

/** Event types: 1–13 appointment, session and config events; 14–15 WebSocket-only message types. */
export const EventType = {
  AppointmentCreated: 1,
  AppointmentRescheduled: 2,
  AppointmentCancelled: 3,
  AppointmentCheckedIn: 4,
  AppointmentStarted: 5,
  AppointmentCompleted: 6,
  AppointmentNoShow: 7,
  AppointmentReinstated: 8,
  AppointmentReassigned: 9,
  AppointmentExtended: 10,
  AppointmentOverrunning: 11,
  SessionRevoked: 12,
  ConfigUpdated: 13,
  AvailabilityChanged: 14,
  Connected: 15,
} as const;

/** What an outbox event, or a live config notice, is about. */
export const AggregateType = {
  Appointment: 1,
  Dealership: 2,
  ServiceType: 3,
  Technician: 4,
  User: 5,
} as const;

export type AppointmentStatus = (typeof Status)[keyof typeof Status];
export type BayType = (typeof Bay)[keyof typeof Bay];
export type TechnicianSkill = (typeof Skill)[keyof typeof Skill];
export type TechnicianSkillLevel = (typeof SkillLevel)[keyof typeof SkillLevel];

/** Every skill level, lowest first (form pickers). */
export const SKILL_LEVELS = Object.values(SkillLevel) as TechnicianSkillLevel[];
export type EventTypeCode = (typeof EventType)[keyof typeof EventType];
export type AggregateTypeCode = (typeof AggregateType)[keyof typeof AggregateType];

export type TechnicianAdmin = {
  id: string;
  dealershipId: string;
  name: string;
  skillLevel: TechnicianSkillLevel;
  active: boolean;
  skills: TechnicianSkill[];
  version: number;
};

/** A poisoned outbox event (group admin, `GET /admin/outbox/failed`); the payload is not exposed. */
export type FailedOutboxEvent = {
  id: string;
  seq: number;
  aggregateType: AggregateTypeCode;
  aggregateId: string;
  eventType: EventTypeCode;
  attempts: number;
  rejections: number;
  lastError: string;
  createdAt: string;
};

const STATUS_NAME: Record<number, string> = {
  1: "confirmed",
  2: "checked_in",
  3: "in_progress",
  4: "completed",
  5: "cancelled",
  6: "no_show",
};

const BAY_NAME: Record<number, string> = {
  1: "express",
  2: "general",
  3: "diagnostic",
  4: "alignment",
};

const SKILL_NAME: Record<number, string> = {
  1: "lubrication",
  2: "brakes",
  3: "general",
};

const ROLE_NAME: Record<number, string> = {
  1: "admin",
  2: "dispatcher",
  3: "customer",
};

export function statusName(code: number): string {
  return STATUS_NAME[code] ?? "unknown";
}

export function bayName(code: number): string {
  return BAY_NAME[code] ?? "unknown";
}

const BAY_PURPOSE: Record<number, string> = {
  [Bay.Express]: "quick lane for short jobs such as oil changes",
  [Bay.General]: "most servicing and repairs",
  [Bay.Diagnostic]: "fault finding with diagnostic equipment",
  [Bay.Alignment]: "wheel alignment rig",
};

/** What a bay type is for, in a few words (the day board's tooltip). */
export function bayPurpose(code: number): string {
  return BAY_PURPOSE[code] ?? "";
}

export function skillName(code: number): string {
  return SKILL_NAME[code] ?? "unknown";
}

export function roleName(code: number): string {
  return ROLE_NAME[code] ?? String(code);
}

export type Dealership = {
  id: string;
  name: string;
  timezone: string;
  openTime: string;
  closeTime: string;
  lunchStart?: string;
  lunchEnd?: string;
  /** Max vehicle arrivals per staging window (0 = unlimited). */
  stagingCapacity: number;
  /** Staging window length: 15, 30 or 60 minutes. */
  stagingWindowMinutes: number;
  policy: DealershipPolicy;
  today: string;
  version: number;
};

/** Customer self-service rules; staff bookings are not bound by them. */
export type DealershipPolicy = {
  customerLeadMinutes: number;
  customerChangeCutoffMinutes: number;
  bookingHorizonDays: number;
  noShowGraceMinutes: number;
  expressMaxMinutes: number;
};

export type ServiceType = {
  id: string;
  code: string;
  name: string;
  durationMinutes: number;
  requiredSkill: TechnicianSkill;
  requiredBayType: BayType;
  minSkillLevel: TechnicianSkillLevel;
  version: number;
};

export type Vehicle = {
  id: string;
  customerId: string;
  customerName: string;
  vin: string;
  make: string;
  model: string;
  year: number;
};

/** GET /vehicles: one page ordered by owner, make, model; customers only ever get their own. */
/** A vehicle owner: self-registered, or a walk-in recorded by staff (no account). "" = none. */
export type Customer = {
  id: string;
  name: string;
  phone: string;
  email: string;
};

/** POST /customers (staff): at least one of phone and email. */
export type CustomerCreate = {
  name: string;
  phone?: string;
  email?: string;
};

export type VehicleQuery = PageRequest & {
  /** Search text (trimmed, ≤ 100 characters), matched literally and case-insensitively against VIN, make, model, and owner name. */
  q?: string;
};

/** Staff customer search: `q` matches name, phone, or email; pages of 1–100 (default 20). */
export type CustomerQuery = PageRequest & {
  q?: string;
};

export type Slot = {
  start: string;
  end: string;
  available: boolean;
  freeBays: number;
  freeTechnicians: number;
  reason?: string;
  /** Booking this start leaves no new idle gap (butts against a booking or an edge): steer customers to it. */
  recommended?: boolean;
};

export type Availability = {
  date: string;
  timezone: string;
  durationMinutes: number;
  /** Dealership closed that day (weekday closing day or dated closure); slots is empty. */
  closed?: boolean;
  closedReason?: string;
  /** Why an open day offers no start at all: its hours are shorter than the job; slots is empty. */
  reason?: "day_too_short";
  slots: Slot[];
};

export type ServiceBay = {
  id: string;
  name: string;
  bayType: BayType;
};

/** One job line of an appointment (1–5 per booking, order kept). */
export type AppointmentLine = {
  lineNo: number;
  serviceTypeId: string;
  code: string;
  name: string;
  durationMinutes: number;
};

export type Appointment = {
  id: string;
  dealershipId: string;
  dealershipName: string;
  customerId: string;
  customerName: string;
  vehicleId: string;
  vehicleLabel: string;
  vin: string;
  /** Primary (first) job line. */
  serviceTypeId: string;
  /** All job lines joined with " + ". */
  serviceName: string;
  /** Booked labour time (sum of lines). */
  durationMinutes: number;
  lines: AppointmentLine[];
  requiredBayType: BayType;
  requiredSkills: TechnicianSkill[];
  minSkillLevel: TechnicianSkillLevel;
  serviceBayId: string;
  serviceBayName: string;
  serviceBayType: BayType;
  technicianId: string;
  technicianName: string;
  startAt: string;
  endAt: string;
  status: AppointmentStatus;
  isWaiter: boolean;
  /** Row version; send back as If-Match. */
  version: number;
  checkedInAt?: string;
  startedAt?: string;
  completedAt?: string;
  /** Set when in-progress work ran past its end and could not be auto-extended. */
  overrunAt?: string;
  createdAt?: string;
  updatedAt?: string;
};

/**
 * One page of a list endpoint (every list is paged: a plain array body plus the X-Next-Cursor
 * header). Pass `nextCursor` back as `cursor`, with the same filters, for the next page; null on
 * the last page.
 */
export type Page<T> = {
  items: T[];
  nextCursor: string | null;
};

/** Where a list page starts: `cursor` from the previous page (omit for the first), `limit` its size. */
export type PageRequest = {
  limit?: number;
  cursor?: string | null;
};

/** One page of GET /appointments. */
export type AppointmentPage = Page<Appointment>;

/**
 * GET /appointments listings: `upcoming` (confirmed and not yet ended, or checked in / in progress
 * whatever its end: the car is on site; soonest first), `past` (the rest; newest first), or every
 * appointment (omitted; newest first).
 */
export type AppointmentWhen = "upcoming" | "past";

export type AppointmentPageQuery = PageRequest & {
  dealershipId?: string;
  customerId?: string;
  vehicleId?: string;
  when?: AppointmentWhen;
};

export type Schedule = {
  date: string;
  timezone: string;
  /** That day's hours (weekday override or defaults). */
  openTime: string;
  closeTime: string;
  /** That day's lunch break (weekday override or defaults); absent when there is none or the day is closed. */
  lunchStart?: string;
  lunchEnd?: string;
  closed?: boolean;
  closedReason?: string;
  bays: ServiceBay[];
  /** Active technicians of that dealership. */
  technicians: Technician[];
  appointments: Appointment[];
};

export type Technician = {
  id: string;
  name: string;
  skillLevel?: TechnicianSkillLevel;
  skills?: TechnicianSkill[];
};

/** One audit-trail entry of an appointment (staff only). */
export type AppointmentChange = {
  version: number;
  eventType: EventTypeCode;
  actorId?: string;
  at: string;
  status: AppointmentStatus;
  startAt: string;
  endAt: string;
  serviceBayId: string;
  technicianId: string;
};

export type NextSlot = {
  /** YYYY-MM-DD in dealership local time. */
  date: string;
  start: string;
  end: string;
  freeBays: number;
  freeTechnicians: number;
  /** As on Slot: booking this start leaves no new idle gap. */
  recommended?: boolean;
};

export type NextAvailability = {
  timezone: string;
  durationMinutes: number;
  searchedFrom: string;
  searchedTo: string;
  slots: NextSlot[];
};

/** Weekly working pattern on one ISO weekday (1 = Monday … 7 = Sunday), local time. */
export type TechnicianShift = {
  weekday: number;
  startTime: string;
  endTime: string;
  breakStart?: string;
  breakEnd?: string;
};

export type TechnicianAbsence = {
  id: string;
  startsAt: string;
  endsAt: string;
  reason?: string;
};

export type TechnicianRoster = {
  technicianId: string;
  /** Technician row version; send back as If-Match. */
  version: number;
  /** Empty = works the dealership's opening hours. */
  shifts: TechnicianShift[];
  absences: TechnicianAbsence[];
};

/** Overrides the default hours on one ISO weekday (1 = Monday … 7 = Sunday). */
export type WeekdayHours = {
  weekday: number;
  closed: boolean;
  openTime?: string;
  closeTime?: string;
  lunchStart?: string;
  lunchEnd?: string;
};

export type Closure = {
  /** YYYY-MM-DD in dealership local time. */
  date: string;
  reason?: string;
};

export type DealershipCalendar = {
  dealershipId: string;
  /** Dealership row version; send back as If-Match. */
  version: number;
  weekly: WeekdayHours[];
  closures: Closure[];
};

/** WebSocket payloads — never contain personal data. */
export type AppointmentNotice = {
  id: string;
  dealershipId: string;
  status: AppointmentStatus;
  version: number;
  startAt: string;
  endAt: string;
};

export type AvailabilityNotice = {
  dealershipId: string;
  /** The changed appointment's window; absent for a technician change (roster, skills), which may affect any day. */
  startAt?: string;
  endAt?: string;
};

export type ConfigNotice = {
  entity: typeof AggregateType.Dealership | typeof AggregateType.ServiceType | typeof AggregateType.Technician;
  id: string;
  version: number;
};

/** A WebSocket frame; `type` is an EventType code. */
export type LiveMessage =
  | { type: typeof EventType.Connected; payload?: unknown }
  | { type: typeof EventType.AvailabilityChanged; payload: AvailabilityNotice }
  | { type: typeof EventType.ConfigUpdated; payload: ConfigNotice }
  | { type: EventTypeCode; payload: AppointmentNotice };
