import { type Ref, useState } from "react";
import { AppointmentStaffTools } from "./AppointmentStaffTools";
import { confirmAction } from "./ConfirmDialog";
import { ExtendControl } from "./ExtendControl";
import { api, type AppointmentAction } from "./api";
import { actionBlock, actionBoundaries, customerCanChange, type TimedAction } from "./booking";
import { statusLabel } from "./labels";
import { formatClockRange, formatDate, formatDayLabel, formatMinutes, zoneAbbr } from "./time";
import { Status, bayName, statusName, type Appointment, type DealershipPolicy, type Schedule } from "./types";
import { useNow } from "./useNow";
import { Icon, statusIcon, type IconName } from "./Icon";

export type ActionOptions = {
  /** Shown as a toast once the action succeeded. */
  success?: string;
};

type Props = {
  appointment: Appointment;
  timezone: string;
  busy: boolean;
  isStaff: boolean;
  /** The appointment's dealership policy (customer change cutoff); undefined while unknown. */
  policy?: DealershipPolicy;
  /** That dealership's day schedule (bays + technicians) for staff reassignment; null if not loaded. */
  schedule: Schedule | null;
  /** The slot grid is currently offering new times for this appointment. */
  rescheduling: boolean;
  onAction: (fn: AppointmentAction, opts?: ActionOptions) => void;
  onStartReschedule: () => void;
  onClose: () => void;
  sectionRef?: Ref<HTMLElement>;
  /** Panel heading: "3. Appointment" in the customer's numbered flow; staff open it from the board. */
  heading?: string;
};

type Action = {
  label: string;
  icon: IconName;
  fn: AppointmentAction;
  success: string;
  primary?: boolean;
  staffOnly?: boolean;
  /** Customer-facing changes the server refuses after the change cutoff. */
  customerChange?: boolean;
  /** Destructive: asked before it is sent. */
  confirm?: string;
  /** Timing the server checks: the button is disabled, with the reason, until it is allowed. */
  timed?: TimedAction;
};

const ACTIONS: Record<number, Action[]> = {
  [Status.Confirmed]: [
    { label: "Check in", icon: "logIn", fn: api.checkIn, success: "Checked in.", primary: true, staffOnly: true, timed: "checkIn" },
    {
      label: "No-show",
      icon: "userX",
      fn: api.noShow,
      success: "Marked as no-show; the bay and technician are free again.",
      staffOnly: true,
      confirm: "Mark this appointment as a no-show? Its bay and technician are released.",
      timed: "noShow",
    },
    {
      label: "Cancel appointment",
      icon: "xCircle",
      fn: api.cancel,
      success: "Appointment cancelled.",
      customerChange: true,
      confirm: "Cancel this appointment? The slot is released and may be booked by someone else.",
    },
  ],
  [Status.CheckedIn]: [
    { label: "Start work", icon: "play", fn: api.start, success: "Work started.", primary: true, staffOnly: true },
    {
      label: "Cancel appointment",
      icon: "xCircle",
      fn: api.cancel,
      success: "Appointment cancelled.",
      staffOnly: true,
      confirm: "Cancel this checked-in appointment? The slot is released.",
    },
  ],
  [Status.InProgress]: [
    {
      label: "Complete (release bay/tech)",
      icon: "flag",
      fn: api.complete,
      success: "Completed; the bay and technician are free.",
      primary: true,
      staffOnly: true,
    },
  ],
  [Status.NoShow]: [
    {
      label: "Reinstate (arrived late)",
      icon: "undo",
      fn: api.reinstate,
      success: "Reinstated and checked in.",
      primary: true,
      staffOnly: true,
      timed: "reinstate",
    },
  ],
};

export function AppointmentPanel({
  appointment,
  timezone,
  busy,
  isStaff,
  policy,
  schedule,
  rescheduling,
  onAction,
  onStartReschedule,
  onClose,
  sectionRef,
  heading = "3. Appointment",
}: Props) {
  // Re-rendered right when a timed action flips (start, end, local midnight), not only each minute.
  const ticked = useNow(actionBoundaries(appointment, timezone));
  // A click re-checks against the real clock and remembers it, so the hint catches up at once.
  const [clicked, setClicked] = useState(0);
  const now = Math.max(ticked, clicked);
  const actions = (ACTIONS[appointment.status] ?? []).filter((a) => isStaff || !a.staffOnly);
  const blockOf = (a: Action) => (a.timed ? actionBlock(a.timed, appointment, timezone, now) : "");
  const blocks = actions.map(blockOf).filter(Boolean);
  // Customers can change online only until the cutoff; staff are never bound by it.
  const locked = !isStaff && !customerCanChange(appointment, policy);
  const canReschedule = appointment.status === Status.Confirmed;
  // Checked-in or overrunning work: staff extend it by a chosen 15–240 minutes.
  const canExtend = isStaff && (appointment.status === Status.CheckedIn || appointment.status === Status.InProgress);

  async function trigger(action: Action) {
    // The rendered verdict may be a moment old: a reinstate just past the booked end would be refused.
    const at = Date.now();
    if (action.timed && actionBlock(action.timed, appointment, timezone, at)) {
      setClicked(at);
      return;
    }
    if (action.confirm && !(await confirmAction(action.confirm))) return;
    onAction(action.fn, { success: action.success });
  }

  return (
    <section className="panel confirm" ref={sectionRef}>
      <div className="panel-head">
        {/* Focusable so the app can move focus here after a booking or opening an appointment. */}
        <h2 tabIndex={-1}>
          <Icon name="calendarCheck" />
          {heading}
        </h2>
        <button type="button" className="link" onClick={onClose}>
          <Icon name="x" />
          Close
        </button>
      </div>
      <dl>
        <div>
          <dt>
            <Icon name="flag" />
            Status
          </dt>
          <dd>
            <span className={`status-badge status-${statusName(appointment.status)}`}>
              <Icon name={statusIcon(appointment.status)} />
              {statusLabel(appointment.status)}
            </span>
            {isStaff ? <small className="hint"> v{appointment.version}</small> : null}
            {appointment.overrunAt && (
              <strong className="overrun">
                {" · "}
                <Icon name="alert" />
                overrunning — next job is blocked
              </strong>
            )}
          </dd>
        </div>
        <div>
          <dt>
            <Icon name="user" />
            Customer
          </dt>
          <dd>{appointment.customerName}</dd>
        </div>
        <div>
          <dt>
            <Icon name="car" />
            Vehicle
          </dt>
          <dd>{appointment.vehicleLabel}</dd>
        </div>
        <div>
          <dt>
            <Icon name="wrench" />
            Service
          </dt>
          <dd>{appointment.serviceName}</dd>
        </div>
        <div>
          <dt>
            <Icon name="clock" />
            When
          </dt>
          <dd>
            {formatDayLabel(formatDate(appointment.startAt, timezone))} ·{" "}
            {formatClockRange(appointment.startAt, appointment.endAt, timezone)}{" "}
            <small className="hint">{zoneAbbr(timezone, appointment.startAt)}</small>
          </dd>
        </div>
        <div>
          <dt>
            <Icon name="bay" />
            Bay
          </dt>
          <dd>
            {appointment.serviceBayName}
            {isStaff ? ` (${bayName(appointment.serviceBayType)})` : ""}
          </dd>
        </div>
        <div>
          <dt>
            <Icon name="hardHat" />
            Technician
          </dt>
          <dd>{appointment.technicianName}</dd>
        </div>
        <div>
          <dt>
            <Icon name="armchair" />
            {isStaff ? "Waiter" : "Waiting on site"}
          </dt>
          <dd>{appointment.isWaiter ? "yes" : "no"}</dd>
        </div>
      </dl>
      {appointment.lines.length > 1 ? (
        <ol className="job-lines">
          {appointment.lines.map((line) => (
            <li key={line.lineNo}>
              {line.name} <small className="hint">({line.durationMinutes}m)</small>
            </li>
          ))}
        </ol>
      ) : null}
      {actions.length > 0 || canReschedule || canExtend ? (
        <div className="actions">
          {actions.map((action) => (
            <button
              key={action.label}
              type="button"
              className={action.primary ? "primary" : action.confirm ? "danger-outline" : undefined}
              disabled={busy || (action.customerChange && locked) || blockOf(action) !== ""}
              title={blockOf(action) || undefined}
              onClick={() => void trigger(action)}
            >
              <Icon name={action.icon} />
              {action.label}
            </button>
          ))}
          {canReschedule ? (
            <button type="button" disabled={busy || locked || rescheduling} onClick={onStartReschedule}>
              <Icon name="reschedule" />
              {rescheduling ? "Choosing a new time…" : "Reschedule…"}
            </button>
          ) : null}
          {canExtend ? (
            <ExtendControl
              busy={busy}
              onExtend={(minutes) =>
                onAction((a) => api.extend(a, minutes), { success: `Extended by ${formatMinutes(minutes)}.` })
              }
            />
          ) : null}
        </div>
      ) : null}
      {blocks.map((text) => (
        <p key={text} className="hint">
          {text}
        </p>
      ))}
      {locked && canReschedule && policy ? (
        <p className="hint">
          <Icon name="lock" />
          Online changes close {formatMinutes(policy.customerChangeCutoffMinutes)} before the start — please contact
          the dealership to change or cancel.
        </p>
      ) : null}
      {isStaff ? (
        <AppointmentStaffTools
          appointment={appointment}
          timezone={timezone}
          busy={busy}
          schedule={schedule}
          onAction={onAction}
        />
      ) : null}
    </section>
  );
}
