import { AdminNotice, StaleDraftNotice } from "./AdminNotice";
import { ErrorBanner } from "./ErrorBanner";
import { dealerHoursForm, hoursPatch, hoursProblem, type DealerHoursForm } from "./admin";
import { api } from "./api";
import type { Dealership, DealershipPolicy } from "./types";
import { useAdminAction } from "./useAdminAction";
import { useDraft } from "./useDraft";
import { Icon } from "./Icon";

const STAGING_WINDOWS = [15, 30, 60] as const;

/** Policy inputs with the backend's accepted ranges. */
const POLICY_FIELDS: { key: keyof DealershipPolicy; label: string; min: number; max: number; hint: string }[] = [
  { key: "customerLeadMinutes", label: "Customer lead time (min)", min: 0, max: 10080, hint: "How much notice a customer must give." },
  { key: "customerChangeCutoffMinutes", label: "Customer change cutoff (min)", min: 0, max: 10080, hint: "Customers can cancel or move online until this long before the start." },
  { key: "bookingHorizonDays", label: "Booking horizon (days)", min: 1, max: 365, hint: "How far ahead customers can book." },
  { key: "noShowGraceMinutes", label: "No-show grace (min)", min: 5, max: 240, hint: "Unarrived bookings become no-shows this long after the start." },
  { key: "expressMaxMinutes", label: "Express bay max job (min)", min: 0, max: 600, hint: "Longest waiter job that may use a free express bay." },
];

type Form = DealerHoursForm & {
  stagingCapacity: number;
  stagingWindowMinutes: 15 | 30 | 60;
  policy: DealershipPolicy;
};

function toForm(d: Dealership): Form {
  return {
    ...dealerHoursForm(d),
    stagingCapacity: d.stagingCapacity,
    stagingWindowMinutes: STAGING_WINDOWS.find((w) => w === d.stagingWindowMinutes) ?? 60,
    policy: { ...d.policy },
  };
}

type Props = {
  dealer: Dealership;
  /** Reloads the catalog (after a save, or when the shown version was stale). */
  onChanged: () => Promise<unknown> | void;
};

/** Default opening hours and lunch, staging cap, and self-service policy of one dealership. */
export function DealershipSettings({ dealer, onChanged }: Props) {
  // A new version (another admin, a live CONFIG_UPDATED) replaces the form only while it has no
  // unsaved edits; edits are kept with a "changed elsewhere" notice, and saving them sends the
  // version they started from (412 instead of overwriting the other change).
  const draft = useDraft<Form>(toForm(dealer), dealer.version, dealer.id);
  const form = draft.value;
  const setForm = (next: Form) => draft.set(() => next);
  const { busy, error, setError, notice, save } = useAdminAction(onChanged);

  async function submit() {
    const problem = hoursProblem(
      form.openTime,
      form.closeTime,
      form.lunch ? form.lunchStart : "",
      form.lunch ? form.lunchEnd : "",
    );
    if (problem) {
      setError(problem);
      return;
    }
    await save(async () => {
      const saved = await api.patchDealership(dealer.id, draft.version, {
        // A stale draft sends its old version and is refused (412), so `dealer` is its base here.
        ...hoursPatch(dealer, form),
        stagingCapacity: form.stagingCapacity,
        stagingWindowMinutes: form.stagingWindowMinutes,
        policy: form.policy,
      });
      draft.rebase(toForm(saved), saved.version);
      await onChanged();
    }, `${dealer.name} saved.`);
  }

  return (
    <div>
      <h3>
        <Icon name="clock" />
        Hours, staging &amp; policy
      </h3>
      <p className="hint">
        Default hours for every weekday (override single weekdays and holidays under Calendar). Times are{" "}
        {dealer.timezone}. A change that would leave upcoming bookings outside the hours is refused — move them first.
      </p>
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />
      {draft.stale ? <StaleDraftNotice what="These settings were" onReload={draft.reload} /> : null}
      <div className="admin-grid">
        <label>
          <span>
            <Icon name="clock" />
            Opens
          </span>
          <input type="time" value={form.openTime} onChange={(e) => setForm({ ...form, openTime: e.target.value })} />
        </label>
        <label>
          <span>
            <Icon name="clock" />
            Closes
          </span>
          <input type="time" value={form.closeTime} onChange={(e) => setForm({ ...form, closeTime: e.target.value })} />
        </label>
        <label className="checkbox">
          <input type="checkbox" checked={form.lunch} onChange={(e) => setForm({ ...form, lunch: e.target.checked })} />
          <Icon name="coffee" className="icon-accent" />
          Lunch break (no bookings overlap it)
        </label>
        {form.lunch ? (
          <>
            <label>
              <span>
                <Icon name="coffee" />
                Lunch starts
              </span>
              <input
                type="time"
                value={form.lunchStart}
                onChange={(e) => setForm({ ...form, lunchStart: e.target.value })}
              />
            </label>
            <label>
              <span>
                <Icon name="coffee" />
                Lunch ends
              </span>
              <input type="time" value={form.lunchEnd} onChange={(e) => setForm({ ...form, lunchEnd: e.target.value })} />
            </label>
          </>
        ) : null}
      </div>
      <div className="admin-grid">
        <label>
          <span>
            <Icon name="car" />
            Staging capacity — arrivals per window (0 = unlimited)
          </span>
          <input
            type="number"
            min={0}
            max={100}
            value={form.stagingCapacity}
            onChange={(e) => setForm({ ...form, stagingCapacity: Number(e.target.value) })}
          />
        </label>
        <label>
          <span>
            <Icon name="clock" />
            Staging window
          </span>
          <select
            value={form.stagingWindowMinutes}
            onChange={(e) =>
              setForm({
                ...form,
                stagingWindowMinutes: STAGING_WINDOWS.find((w) => w === Number(e.target.value)) ?? 60,
              })
            }
          >
            {STAGING_WINDOWS.map((w) => (
              <option key={w} value={w}>
                {w} minutes
              </option>
            ))}
          </select>
        </label>
        {POLICY_FIELDS.map((f) => (
          <label key={f.key} title={f.hint}>
            {f.label}
            <input
              type="number"
              min={f.min}
              max={f.max}
              value={form.policy[f.key]}
              aria-describedby={`policy-${f.key}`}
              onChange={(e) => setForm({ ...form, policy: { ...form.policy, [f.key]: Number(e.target.value) } })}
            />
            <small id={`policy-${f.key}`} className="hint">
              {f.hint}
            </small>
          </label>
        ))}
      </div>
      <button type="button" className="primary" disabled={busy} onClick={submit}>
        <Icon name="check" />
        {busy ? "Saving…" : "Save dealership"}
      </button>
    </div>
  );
}
