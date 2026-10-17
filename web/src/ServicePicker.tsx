import { useState } from "react";
import { MAX_JOB_LINES, jobLineBlock, pickJobLine } from "./booking";
import { formatMinutes } from "./time";
import { bayName, skillName, type ServiceType } from "./types";
import { Icon, skillIcon } from "./Icon";

type Props = {
  services: ServiceType[];
  /** Job lines in pick order; the first is the primary service. */
  selected: string[];
  isWaiter: boolean;
  /** Staff see the bay type and skill each service needs; customers only its name and duration. */
  isStaff?: boolean;
  onChange: (ids: string[]) => void;
  /** Locked while rescheduling (the appointment's booked job lines). */
  disabled?: boolean;
};

/**
 * Pick 1–5 job lines for one visit; they are booked back to back on one bay and technician. With
 * one line the choices are radio buttons (a click swaps the service); "Add another service" turns
 * them into checkboxes for a multi-line visit.
 */
export function ServicePicker({ services, selected, isWaiter, isStaff = false, onChange, disabled = false }: Props) {
  const [addMode, setAddMode] = useState(false);
  const multi = addMode || selected.length > 1;
  const lines = selected
    .map((id) => services.find((s) => s.id === id))
    .filter((s): s is ServiceType => Boolean(s));
  const total = lines.reduce((sum, s) => sum + s.durationMinutes, 0);

  return (
    <fieldset className="service-picker" disabled={disabled}>
      <legend>
        <Icon name="wrench" />
        {multi
          ? isStaff
            ? `Services (job lines, up to ${MAX_JOB_LINES}; the first picked is the primary)`
            : `Services (pick up to ${MAX_JOB_LINES})`
          : "Service"}
      </legend>
      {services.map((s) => {
        const index = selected.indexOf(s.id);
        const checked = index >= 0;
        // Radio-like (one line): any other service can be picked; the ticked one stays ticked.
        const block = multi ? jobLineBlock(selected, s.id) : "";
        return (
          <label key={s.id} className="checkbox" title={block || undefined}>
            <input
              type={multi ? "checkbox" : "radio"}
              name={multi ? undefined : "service-line"}
              checked={checked}
              disabled={disabled || block !== ""}
              onChange={() => onChange(pickJobLine(selected, s.id, multi))}
            />
            {checked && selected.length > 1 ? <strong>#{index + 1}</strong> : null}
            <Icon name={skillIcon(s.requiredSkill)} className="icon-accent" />
            {s.name} ({formatMinutes(s.durationMinutes)}
            {isStaff ? ` · ${bayName(s.requiredBayType)} · ${skillName(s.requiredSkill)} L${s.minSkillLevel}+` : ""})
          </label>
        );
      })}
      {!multi && selected.length === 1 && services.length > 1 ? (
        <button type="button" className="link" disabled={disabled} onClick={() => setAddMode(true)}>
          <Icon name="plus" />
          Add another service
        </button>
      ) : null}
      {lines.length > 0 ? (
        <p className="hint">
          {isStaff
            ? `${lines.length > 1 ? `${lines.length} job lines · ` : ""}labour ${total} minutes${
                lines.length > 1 ? " · one bay and technician that fit every line" : ""
              }${isWaiter ? " · waiter soft-upgrade to express when free" : ""}`
            : `About ${formatMinutes(total)} in the workshop${lines.length > 1 ? `, ${lines.length} services in one visit` : ""}.`}
        </p>
      ) : (
        <p className="hint">{isStaff ? "Pick the job for this booking." : "Pick a service."}</p>
      )}
    </fieldset>
  );
}
