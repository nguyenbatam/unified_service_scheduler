import { useState } from "react";
import { formatMinutes } from "./time";
import { Icon } from "./Icon";

/** The API's bounds for one extension: 15–240 minutes in 15-minute steps. */
export const EXTEND_MIN = 15;
export const EXTEND_MAX = 240;
export const EXTEND_STEP = 15;
const PRESETS = [15, 30, 60] as const;

/** Why `minutes` cannot be sent as an extension ("" = it can). */
export function extendProblem(minutes: number): string {
  if (!Number.isInteger(minutes) || minutes < EXTEND_MIN || minutes > EXTEND_MAX || minutes % EXTEND_STEP !== 0) {
    return `Extend by ${EXTEND_MIN}–${EXTEND_MAX} minutes, in steps of ${EXTEND_STEP}.`;
  }
  return "";
}

type Props = {
  busy: boolean;
  /** Sends the extension (If-Match: the shown version). */
  onExtend: (minutes: number) => void;
};

/** Staff: extend checked-in or overrunning work by 15, 30, 60 minutes, or a custom 15–240. */
export function ExtendControl({ busy, onExtend }: Props) {
  const [choice, setChoice] = useState<string>("15");
  const [custom, setCustom] = useState("90");
  const minutes = choice === "custom" ? Number(custom) : Number(choice);
  const problem = extendProblem(minutes);
  return (
    <span className="extend-control">
      <select aria-label="Extend by" value={choice} disabled={busy} onChange={(e) => setChoice(e.target.value)}>
        {PRESETS.map((m) => (
          <option key={m} value={String(m)}>
            +{formatMinutes(m)}
          </option>
        ))}
        <option value="custom">Other…</option>
      </select>
      {choice === "custom" ? (
        <input
          type="number"
          aria-label="Extend minutes"
          min={EXTEND_MIN}
          max={EXTEND_MAX}
          step={EXTEND_STEP}
          value={custom}
          disabled={busy}
          onChange={(e) => setCustom(e.target.value)}
        />
      ) : null}
      <button type="button" disabled={busy || problem !== ""} title={problem || undefined} onClick={() => onExtend(minutes)}>
        <Icon name="clock" />
        {problem ? "Extend" : `Extend +${formatMinutes(minutes)}`}
      </button>
    </span>
  );
}
