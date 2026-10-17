import { useCallback, useEffect, useRef, useState } from "react";
import { AdminNotice, StaleDraftNotice } from "./AdminNotice";
import { CalendarEditor } from "./CalendarEditor";
import { confirmAction } from "./ConfirmDialog";
import { DealershipSettings } from "./DealershipSettings";
import { ErrorBanner } from "./ErrorBanner";
import { OutboxPanel } from "./OutboxPanel";
import { ProvisioningPanel } from "./ProvisioningPanel";
import { RosterEditor } from "./RosterEditor";
import { Tabs } from "./Tabs";
import { TruncatedHint } from "./TruncatedHint";
import { adminDealerships, adminTabs, isGroupAdmin, removedSkills, sameSkills, type AdminTab } from "./admin";
import { api, type Catalog } from "./api";
import { errorText } from "./errors";
import { currentSession } from "./auth";
import { useAdminAction } from "./useAdminAction";
import { useDraft } from "./useDraft";
import { skillLevelLabel } from "./labels";
import {
  Bay,
  SKILL_LEVELS,
  Skill,
  bayName,
  skillName,
  type AuthUser,
  type BayType,
  type Dealership,
  type ServiceType,
  type TechnicianAdmin,
  type TechnicianRoster,
  type TechnicianSkill,
} from "./types";
import { Icon, type IconName } from "./Icon";

const TAB_ICON: Record<AdminTab, IconName> = {
  settings: "clock",
  calendar: "calendar",
  technicians: "hardHat",
  capacity: "plus",
  catalog: "wrench",
  outbox: "alert",
};

const TAB_LABEL: Record<AdminTab, string> = {
  settings: "Hours & policy",
  calendar: "Calendar",
  technicians: "Technicians",
  capacity: "Add capacity",
  catalog: "Service catalog",
  outbox: "Failed events",
};

type Props = {
  dealerships: Dealership[];
  services: ServiceType[];
  onChanged: () => Promise<unknown> | void;
  /** The signed-in admin (defaults to the stored session): decides the dealerships and tabs shown. */
  user?: AuthUser;
  /** The catalog lists that stopped at FETCH_ALL_MAX (their dropdowns say so). */
  truncated?: Catalog["truncated"];
};

export function AdminPanel({ dealerships, services, onChanged, user = currentSession()?.user, truncated }: Props) {
  const group = isGroupAdmin(user);
  const scoped = adminDealerships(user, dealerships);
  const tabs = adminTabs(group);
  const [tab, setTab] = useState<AdminTab>("settings");
  const [dealerID, setDealerID] = useState(scoped[0]?.id ?? "");
  const [techs, setTechs] = useState<TechnicianAdmin[] | null>(null);
  const [techsTruncated, setTechsTruncated] = useState(false);
  const [techError, setTechError] = useState("");

  const dealer = scoped.find((d) => d.id === dealerID) ?? scoped[0];

  // Only the latest load applies: switching dealership quickly never shows (and edits) the
  // previous dealership's technicians under the new one.
  const techSeq = useRef(0);
  const loadTechs = useCallback(async () => {
    if (!dealer) return;
    const mine = ++techSeq.current;
    try {
      const list = await api.adminTechnicians(dealer.id);
      if (mine !== techSeq.current) return;
      setTechs(list.items);
      setTechsTruncated(list.truncated);
      setTechError("");
    } catch (err) {
      if (mine === techSeq.current) setTechError(errorText(err));
    }
  }, [dealer?.id]);

  useEffect(() => {
    setTechs(null);
    void loadTechs();
  }, [loadTechs]);

  if (!dealer) {
    return (
      <section className="panel">
        <h2>
          <Icon name="settings" />
          Admin
        </h2>
        <p className="hint">
          {user?.dealershipId
            ? "Your dealership is not in the catalog yet — reload the page."
            : "No dealerships yet."}
        </p>
        {group ? (
          <OutboxPanel />
        ) : null}
      </section>
    );
  }

  // Roster changes bump the technician version; keep the list's If-Match current.
  function rosterSaved(roster: TechnicianRoster) {
    setTechs((prev) => (prev ?? []).map((t) => (t.id === roster.technicianId ? { ...t, version: roster.version } : t)));
  }

  return (
    <section className="panel">
      <div className="admin-head">
        <h2>
          <Icon name="settings" />
          Admin
        </h2>
        {scoped.length > 1 ? (
          <>
            <label>
              <span>
                <Icon name="dealership" />
                Dealership
              </span>
              <select value={dealer.id} onChange={(e) => setDealerID(e.target.value)}>
                {scoped.map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name}
                  </option>
                ))}
              </select>
            </label>
            <TruncatedHint shown={truncated?.dealerships ?? false} what="dealerships" />
          </>
        ) : (
          <p className="hint">
            {dealer.name} · {group ? "group admin" : "dealership admin"}
          </p>
        )}
      </div>

      <Tabs
        label="Admin sections"
        idPrefix="admin"
        className="admin-tabs"
        tabs={tabs.map((t) => ({ id: t, label: TAB_LABEL[t], icon: TAB_ICON[t] }))}
        active={tab}
        onChange={setTab}
      >
        {tab === "settings" ? <DealershipSettings dealer={dealer} onChanged={onChanged} /> : null}
        {tab === "calendar" ? <CalendarEditor dealer={dealer} onChanged={onChanged} /> : null}
        {tab === "technicians" ? (
          <>
            <ErrorBanner message={techError} />
            {techs === null ? (
              techError ? null : <p className="hint">Loading technicians…</p>
            ) : (
              <>
                <TruncatedHint shown={techsTruncated} what="technicians" />
                <TechnicianList
                  techs={techs}
                  onUpdated={(u) => setTechs((prev) => (prev ?? []).map((x) => (x.id === u.id ? u : x)))}
                  onStale={loadTechs}
                />
                {techs.length > 0 ? (
                  <RosterEditor technicians={techs} dealerships={scoped} onSaved={rosterSaved} />
                ) : null}
              </>
            )}
          </>
        ) : null}
        {tab === "capacity" ? (
          <ProvisioningPanel
            dealer={dealer}
            canCreateDealership={group}
            onChanged={onChanged}
            onTechnicianCreated={(t) => setTechs((prev) => [...(prev ?? []), t])}
          />
        ) : null}
        {tab === "catalog" && group ? <ServiceCatalog services={services} truncated={truncated?.services ?? false} onChanged={onChanged} /> : null}
        {tab === "outbox" && group ? <OutboxPanel /> : null}
      </Tabs>
    </section>
  );
}

type TechListProps = {
  techs: TechnicianAdmin[];
  onUpdated: (t: TechnicianAdmin) => void;
  onStale: () => Promise<unknown>;
};

/** Skill level, skills and active flag per technician. */
function TechnicianList({ techs, onUpdated, onStale }: TechListProps) {
  const [levels, setLevels] = useState<Record<string, number>>({});
  const [skillDrafts, setSkillDrafts] = useState<Record<string, TechnicianSkill[]>>({});
  const { busy, error, setError, notice, save } = useAdminAction(onStale);

  function toggleSkill(t: TechnicianAdmin, skill: TechnicianSkill, on: boolean) {
    setSkillDrafts((prev) => {
      const current = prev[t.id] ?? t.skills;
      const next = on ? [...current, skill] : current.filter((x) => x !== skill);
      return { ...prev, [t.id]: SKILLS.filter((x) => next.includes(x)) };
    });
  }

  async function saveSkills(t: TechnicianAdmin) {
    const skills = skillDrafts[t.id];
    if (!skills || sameSkills(skills, t.skills)) return;
    if (skills.length === 0) {
      setError(`${t.name} needs at least one skill.`);
      return;
    }
    const removed = removedSkills(t.skills, skills);
    if (
      removed.length > 0 &&
      !(await confirmAction(
        `Remove ${removed.map(skillName).join(", ")} from ${t.name}? New bookings needing it will not go to them. This is refused while an upcoming appointment of theirs needs it — reassign those first.`,
      ))
    ) {
      return;
    }
    const ok = await save(
      async () => onUpdated(await api.patchTechnician(t.id, t.version, { skills })),
      `${t.name}'s skills saved.`,
    );
    if (ok) setSkillDrafts(({ [t.id]: _, ...rest }) => rest);
  }

  async function saveLevel(t: TechnicianAdmin) {
    const skillLevel = levels[t.id];
    if (!skillLevel || skillLevel === t.skillLevel) return;
    const ok = await save(async () => onUpdated(await api.patchTechnician(t.id, t.version, { skillLevel })), `${t.name} is now ${skillLevelLabel(skillLevel)}.`);
    if (ok) setLevels(({ [t.id]: _, ...rest }) => rest);
  }

  async function toggle(t: TechnicianAdmin) {
    if (
      t.active &&
      !(await confirmAction(
        `Deactivate ${t.name}? They will not be offered for new bookings. This is refused while they still have upcoming appointments — reassign those first.`,
      ))
    ) {
      return;
    }
    await save(
      async () => onUpdated(await api.patchTechnician(t.id, t.version, { active: !t.active })),
      `${t.name} ${t.active ? "deactivated" : "activated"}.`,
    );
  }

  return (
    <div>
      <h3>
        <Icon name="hardHat" />
        Technicians
      </h3>
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />
      {techs.length === 0 ? <p className="hint">No technicians yet — add one under Add capacity.</p> : null}
      <div className="tech-list">
        {techs.map((t) => {
          const level = levels[t.id] ?? t.skillLevel;
          const skills = skillDrafts[t.id] ?? t.skills;
          return (
            <div key={t.id} className="tech-row">
              <span>
                <strong>{t.name}</strong>
                {t.active ? "" : " (inactive)"}
              </span>
              <span className="row-actions" role="group" aria-label={`${t.name} skills`}>
                {SKILLS.map((sk) => (
                  <label key={sk} className="checkbox">
                    <input
                      type="checkbox"
                      checked={skills.includes(sk)}
                      disabled={busy}
                      onChange={(e) => toggleSkill(t, sk, e.target.checked)}
                    />
                    {skillName(sk)}
                  </label>
                ))}
                {!sameSkills(skills, t.skills) ? (
                  <button type="button" className="primary" disabled={busy} onClick={() => saveSkills(t)}>
                    <Icon name="check" />
                    Save skills
                  </button>
                ) : null}
              </span>
              <span className="row-actions">
                <select
                  aria-label={`${t.name} skill level`}
                  value={level}
                  disabled={busy}
                  onChange={(e) => setLevels((prev) => ({ ...prev, [t.id]: Number(e.target.value) }))}
                >
                  {SKILL_LEVELS.map((l) => (
                    <option key={l} value={l}>
                      {skillLevelLabel(l)}
                    </option>
                  ))}
                </select>
                {level !== t.skillLevel ? (
                  <button type="button" className="primary" disabled={busy} onClick={() => saveLevel(t)}>
                    <Icon name="check" />
                    Save level
                  </button>
                ) : null}
                <button type="button" disabled={busy} onClick={() => toggle(t)}>
                  <Icon name={t.active ? "pause" : "play"} />
                  {t.active ? "Deactivate" : "Activate"}
                </button>
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

const BAY_TYPES = Object.values(Bay) as BayType[];
const SKILLS = Object.values(Skill) as TechnicianSkill[];

type ServiceFields = { duration: number; minLevel: number; skill: TechnicianSkill; bayType: BayType };

function serviceFields(s: ServiceType | undefined): ServiceFields {
  return {
    duration: s?.durationMinutes ?? 30,
    minLevel: s?.minSkillLevel ?? 1,
    skill: (s?.requiredSkill ?? Skill.General) as TechnicianSkill,
    bayType: (s?.requiredBayType ?? Bay.General) as BayType,
  };
}

type CatalogProps = {
  services: ServiceType[];
  /** The catalog stopped at FETCH_ALL_MAX. */
  truncated: boolean;
  onChanged: () => Promise<unknown> | void;
};

/** Group admin: the shared service catalog. Changes apply to new bookings only. */
function ServiceCatalog({ services, truncated, onChanged }: CatalogProps) {
  const [serviceID, setServiceID] = useState(services[0]?.id ?? "");
  const service = services.find((x) => x.id === serviceID);
  // A draft of the service's fields: a live CONFIG_UPDATED replaces it only while unedited; edits
  // are kept with a "changed elsewhere" notice and saved with the version they started from.
  const draft = useDraft<ServiceFields>(serviceFields(service), service?.version ?? 0, service?.id ?? "");
  const { duration, minLevel, skill, bayType } = draft.value;
  const setField = (patch: Partial<ServiceFields>) => draft.set((f) => ({ ...f, ...patch }));
  const { busy, error, setError, notice, save } = useAdminAction(onChanged);

  async function submit() {
    if (!service) return;
    if (duration < 30 || duration % 30 !== 0) {
      setError("Duration must be a multiple of 30 minutes.");
      return;
    }
    await save(async () => {
      const saved = await api.patchServiceType(service.id, draft.version, {
        durationMinutes: duration,
        minSkillLevel: minLevel,
        requiredSkill: skill,
        requiredBayType: bayType,
      });
      draft.rebase(serviceFields(saved), saved.version);
      await onChanged();
    }, `${service.name} saved. Existing bookings keep what they were booked with.`);
  }

  return (
    <div>
      <h3>
        <Icon name="wrench" />
        Service catalog
      </h3>
      <p className="hint">Shared by every dealership. Changes apply to new bookings only.</p>
      <TruncatedHint shown={truncated} what="services" />
      <ErrorBanner message={error} />
      <AdminNotice message={notice} />
      {draft.stale ? <StaleDraftNotice what="This service was" onReload={draft.reload} /> : null}
      <div className="admin-grid">
        <label>
          <span>
            <Icon name="wrench" />
            Service
          </span>
          <select value={serviceID} onChange={(e) => setServiceID(e.target.value)}>
            {services.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name} ({bayName(s.requiredBayType)} · {skillName(s.requiredSkill)})
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>
            <Icon name="clock" />
            Duration (minutes, ×30)
          </span>
          <input type="number" min={30} step={30} value={duration} onChange={(e) => setField({ duration: Number(e.target.value) })} />
        </label>
        <label>
          <span>
            <Icon name="hardHat" />
            Min skill level
          </span>
          <select value={minLevel} onChange={(e) => setField({ minLevel: Number(e.target.value) })}>
            {SKILL_LEVELS.map((l) => (
              <option key={l} value={l}>
                {skillLevelLabel(l)}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>
            <Icon name="hardHat" />
            Required skill
          </span>
          <select value={skill} onChange={(e) => setField({ skill: Number(e.target.value) as TechnicianSkill })}>
            {SKILLS.map((s) => (
              <option key={s} value={s}>
                {skillName(s)}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>
            <Icon name="bay" />
            Bay type
          </span>
          <select value={bayType} onChange={(e) => setField({ bayType: Number(e.target.value) as BayType })}>
            {BAY_TYPES.map((b) => (
              <option key={b} value={b}>
                {bayName(b)}
              </option>
            ))}
          </select>
        </label>
      </div>
      <button type="button" className="primary" disabled={busy || !service} onClick={submit}>
        <Icon name="check" />
        {busy ? "Saving…" : "Save service"}
      </button>
    </div>
  );
}
