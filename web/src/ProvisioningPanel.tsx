import { useMemo, useState } from "react";
import { AdminNotice } from "./AdminNotice";
import { ErrorBanner } from "./ErrorBanner";
import { hoursProblem } from "./admin";
import { api } from "./api";
import { skillLevelLabel } from "./labels";
import { Bay, SKILL_LEVELS, Skill, bayName, skillName, type BayType, type Dealership, type TechnicianAdmin, type TechnicianSkill } from "./types";
import { useAdminAction } from "./useAdminAction";
import { Icon, skillIcon } from "./Icon";

const BAY_TYPES = Object.values(Bay) as BayType[];
const SKILLS = Object.values(Skill) as TechnicianSkill[];

function timeZones(): string[] {
  try {
    return Intl.supportedValuesOf("timeZone");
  } catch {
    return ["Europe/London", "Europe/Paris", "America/New_York", "Asia/Ho_Chi_Minh", "Australia/Sydney"];
  }
}

type Props = {
  /** Selected dealership: new bays and technicians go here. */
  dealer: Dealership;
  /** Only a group admin opens dealerships. */
  canCreateDealership: boolean;
  /** Reload the catalog (a new dealership or bay changes it). */
  onChanged: () => Promise<unknown> | void;
  onTechnicianCreated: (t: TechnicianAdmin) => void;
};

/** Admin: add capacity — a bay or a technician at the selected dealership, or a new dealership. */
export function ProvisioningPanel({ dealer, canCreateDealership, onChanged, onTechnicianCreated }: Props) {
  const zones = useMemo(timeZones, []);
  const { busy, error, setError, notice, save } = useAdminAction();

  const [bayNameInput, setBayName] = useState("");
  const [bayType, setBayType] = useState<BayType>(Bay.General);

  const [techName, setTechName] = useState("");
  const [techLevel, setTechLevel] = useState(1);
  const [techSkills, setTechSkills] = useState<TechnicianSkill[]>([Skill.General]);

  const [dName, setDName] = useState("");
  const [dZone, setDZone] = useState(dealer.timezone);
  const [dOpen, setDOpen] = useState("08:00");
  const [dClose, setDClose] = useState("17:00");
  const [dLunch, setDLunch] = useState(true);
  const [dLunchStart, setDLunchStart] = useState("12:00");
  const [dLunchEnd, setDLunchEnd] = useState("13:00");

  async function addBay() {
    const name = bayNameInput.trim();
    if (!name) return;
    const ok = await save(async () => {
      await api.createBay(dealer.id, { name, bayType });
      await onChanged();
    }, `Bay “${name}” (${bayName(bayType)}) added to ${dealer.name}.`);
    if (ok) setBayName("");
  }

  async function addTechnician() {
    const name = techName.trim();
    if (!name) return;
    if (techSkills.length === 0) {
      setError("Pick at least one skill.");
      return;
    }
    const ok = await save(async () => {
      onTechnicianCreated(
        await api.createTechnician({ dealershipId: dealer.id, name, skillLevel: techLevel, skills: techSkills }),
      );
    }, `${name} added to ${dealer.name}; they work the opening hours until you set a roster.`);
    if (ok) setTechName("");
  }

  async function addDealership() {
    const name = dName.trim();
    if (!name) return;
    const problem = hoursProblem(dOpen, dClose, dLunch ? dLunchStart : "", dLunch ? dLunchEnd : "");
    if (problem) {
      setError(problem);
      return;
    }
    if (!zones.includes(dZone)) {
      setError("Pick a time zone from the list, e.g. Europe/London.");
      return;
    }
    const ok = await save(async () => {
      await api.createDealership({
        name,
        timezone: dZone,
        openTime: dOpen,
        closeTime: dClose,
        ...(dLunch ? { lunchStart: dLunchStart, lunchEnd: dLunchEnd } : {}),
      });
      await onChanged();
    }, `${name} opened. Add its bays and technicians next.`);
    if (ok) setDName("");
  }

  function toggleSkill(s: TechnicianSkill, on: boolean) {
    setTechSkills((prev) => (on ? [...prev, s].sort((a, b) => a - b) : prev.filter((x) => x !== s)));
  }

  return (
    <div>
      <h3>
        <Icon name="plus" />
        Add capacity at {dealer.name}
      </h3>
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />

      <div className="inline-form">
        <label>
          <span>
            <Icon name="bay" />
            Bay name
          </span>
          <input type="text" value={bayNameInput} maxLength={100} onChange={(e) => setBayName(e.target.value)} />
        </label>
        <label>
          <span>
            <Icon name="bay" />
            Bay type
          </span>
          <select value={bayType} onChange={(e) => setBayType(Number(e.target.value) as BayType)}>
            {BAY_TYPES.map((b) => (
              <option key={b} value={b}>
                {bayName(b)}
              </option>
            ))}
          </select>
        </label>
        <button type="button" disabled={busy || !bayNameInput.trim()} onClick={addBay}>
          <Icon name="bay" />
          Add bay
        </button>
      </div>

      <div className="inline-form">
        <label>
          <span>
            <Icon name="hardHat" />
            Technician name
          </span>
          <input type="text" value={techName} maxLength={100} onChange={(e) => setTechName(e.target.value)} />
        </label>
        <label>
          <span>
            <Icon name="hardHat" />
            Skill level
          </span>
          <select value={techLevel} onChange={(e) => setTechLevel(Number(e.target.value))}>
            {SKILL_LEVELS.map((l) => (
              <option key={l} value={l}>
                {skillLevelLabel(l)}
              </option>
            ))}
          </select>
        </label>
        <fieldset className="skill-boxes">
          <legend>
            <Icon name="hardHat" />
            Skills
          </legend>
          {SKILLS.map((s) => (
            <label key={s} className="checkbox">
              <input type="checkbox" checked={techSkills.includes(s)} onChange={(e) => toggleSkill(s, e.target.checked)} />
              <Icon name={skillIcon(s)} className="icon-accent" />
              {skillName(s)}
            </label>
          ))}
        </fieldset>
        <button type="button" disabled={busy || !techName.trim()} onClick={addTechnician}>
          <Icon name="hardHat" />
          Add technician
        </button>
      </div>

      {canCreateDealership ? (
        <>
          <h3>
            <Icon name="dealership" />
            Open a new dealership
          </h3>
          <div className="inline-form">
            <label>
              <span>
                <Icon name="dealership" />
                Name
              </span>
              <input type="text" value={dName} maxLength={100} onChange={(e) => setDName(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="globe" />
                Time zone
              </span>
              <input
                type="text"
                list="admin-time-zones"
                value={dZone}
                onChange={(e) => setDZone(e.target.value)}
              />
              <datalist id="admin-time-zones">
                {zones.map((z) => (
                  <option key={z} value={z} />
                ))}
              </datalist>
            </label>
            <label>
              <span>
                <Icon name="clock" />
                Opens
              </span>
              <input type="time" value={dOpen} onChange={(e) => setDOpen(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="clock" />
                Closes
              </span>
              <input type="time" value={dClose} onChange={(e) => setDClose(e.target.value)} />
            </label>
            <label className="checkbox">
              <input type="checkbox" checked={dLunch} onChange={(e) => setDLunch(e.target.checked)} />
              <Icon name="coffee" className="icon-accent" />
              Lunch break
            </label>
            {dLunch ? (
              <>
                <label>
                  <span>
                    <Icon name="coffee" />
                    Lunch starts
                  </span>
                  <input type="time" value={dLunchStart} onChange={(e) => setDLunchStart(e.target.value)} />
                </label>
                <label>
                  <span>
                    <Icon name="coffee" />
                    Lunch ends
                  </span>
                  <input type="time" value={dLunchEnd} onChange={(e) => setDLunchEnd(e.target.value)} />
                </label>
              </>
            ) : null}
            <button type="button" disabled={busy || !dName.trim()} onClick={addDealership}>
              <Icon name="dealership" />
              Open dealership
            </button>
          </div>
          <p className="hint">Staging and self-service policy start at the defaults; edit them afterwards.</p>
        </>
      ) : null}
    </div>
  );
}
