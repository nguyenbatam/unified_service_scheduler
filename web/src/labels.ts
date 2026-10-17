// Human-readable text for the API's machine codes (slot reasons, statuses, history events).

import { AggregateType, EventType, SkillLevel } from "./types";

const SLOT_REASON_TEXT: Record<string, string> = {
  past: "Already passed",
  lead_time: "Too soon to book online",
  beyond_horizon: "Too far ahead to book online",
  lunch_break: "Lunch break",
  vehicle_busy: "Your car is booked then",
  staging_full: "Check-in area full",
  no_bay: "No suitable bay free",
  no_technician: "No qualified technician free",
  no_resources: "Fully booked",
};

/** Staff book for someone else: the reasons that speak to "you" are worded for the desk. */
const STAFF_SLOT_REASON_TEXT: Record<string, string> = {
  vehicle_busy: "This car is already booked then",
};

/** Text for an unavailable slot's `reason` (unknown codes are shown as sent), worded for staff or the customer. */
export function slotReasonText(reason: string | undefined, staff = false): string {
  if (!reason) return "Unavailable";
  return (staff ? STAFF_SLOT_REASON_TEXT[reason] : undefined) ?? SLOT_REASON_TEXT[reason] ?? reason.replace(/_/g, " ");
}

const STATUS_LABEL: Record<number, string> = {
  1: "Confirmed",
  2: "Checked in",
  3: "In progress",
  4: "Completed",
  5: "Cancelled",
  6: "No-show",
};

/** Display label for an appointment status code (statusName stays the CSS/class key). */
export function statusLabel(code: number): string {
  return STATUS_LABEL[code] ?? "Unknown";
}

const EVENT_LABEL: Record<number, string> = {
  [EventType.AppointmentCreated]: "Booked",
  [EventType.AppointmentRescheduled]: "Rescheduled",
  [EventType.AppointmentCancelled]: "Cancelled",
  [EventType.AppointmentCheckedIn]: "Checked in",
  [EventType.AppointmentStarted]: "Work started",
  [EventType.AppointmentCompleted]: "Completed",
  [EventType.AppointmentExtended]: "Extended",
  [EventType.AppointmentOverrunning]: "Overrunning",
  [EventType.AppointmentReassigned]: "Reassigned",
  [EventType.AppointmentNoShow]: "Marked no-show",
  [EventType.AppointmentReinstated]: "Reinstated (arrived late)",
  [EventType.SessionRevoked]: "Session revoked",
  [EventType.ConfigUpdated]: "Configuration changed",
};

/** Display label for an event type code (audit trail, outbox). */
export function eventLabel(eventType: number): string {
  return EVENT_LABEL[eventType] ?? `Event ${eventType}`;
}

const AGGREGATE_LABEL: Record<number, string> = {
  [AggregateType.Appointment]: "Appointment",
  [AggregateType.Dealership]: "Dealership",
  [AggregateType.ServiceType]: "Service type",
  [AggregateType.Technician]: "Technician",
  [AggregateType.User]: "User",
};

/** Display label for an outbox aggregate type code. */
export function aggregateLabel(aggregateType: number): string {
  return AGGREGATE_LABEL[aggregateType] ?? `Aggregate ${aggregateType}`;
}

const SKILL_LEVEL_LABEL: Record<number, string> = {
  [SkillLevel.Apprentice]: "Apprentice",
  [SkillLevel.Technician]: "Technician",
  [SkillLevel.Senior]: "Senior",
  [SkillLevel.Master]: "Master",
};

/** Display label for a skill level code ("L2 Technician"). */
export function skillLevelLabel(level: number): string {
  const name = SKILL_LEVEL_LABEL[level];
  return name ? `L${level} ${name}` : `L${level}`;
}
