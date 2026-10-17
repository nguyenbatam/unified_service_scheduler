import { type KeyboardEvent, type ReactNode, useRef } from "react";
import { Icon, type IconName } from "./Icon";

type TabsProps<T extends string> = {
  /** Accessible name of the tab list. */
  label: string;
  /** Prefix of the tab and panel ids (unique on the page). */
  idPrefix: string;
  tabs: { id: T; label: string; icon?: IconName }[];
  active: T;
  onChange: (id: T) => void;
  className?: string;
  /** The active tab's content, rendered in its tabpanel. */
  children: ReactNode;
};

/**
 * The WAI-ARIA tabs pattern with automatic activation: one tab stop (roving tabindex), arrow keys
 * move between tabs (wrapping), Home/End jump to the first/last, and the panel is labelled by its tab.
 */
export function Tabs<T extends string>({ label, idPrefix, tabs, active, onChange, className = "tabs", children }: TabsProps<T>) {
  const refs = useRef(new Map<T, HTMLButtonElement>());
  const tabId = (id: T) => `${idPrefix}-tab-${id}`;
  const panelId = (id: T) => `${idPrefix}-panel-${id}`;

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    const i = tabs.findIndex((t) => t.id === active);
    let next = -1;
    if (e.key === "ArrowRight") next = (i + 1) % tabs.length;
    else if (e.key === "ArrowLeft") next = (i - 1 + tabs.length) % tabs.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = tabs.length - 1;
    if (next < 0) return;
    e.preventDefault();
    const id = tabs[next].id;
    onChange(id);
    refs.current.get(id)?.focus();
  }

  return (
    <>
      <div className={className} role="tablist" aria-label={label} onKeyDown={onKeyDown}>
        {tabs.map((t) => (
          <button
            key={t.id}
            ref={(el) => {
              if (el) refs.current.set(t.id, el);
              else refs.current.delete(t.id);
            }}
            type="button"
            role="tab"
            id={tabId(t.id)}
            aria-selected={t.id === active}
            aria-controls={panelId(t.id)}
            tabIndex={t.id === active ? 0 : -1}
            className={t.id === active ? "active" : ""}
            onClick={() => onChange(t.id)}
          >
            {t.icon ? <Icon name={t.icon} /> : null}
            {t.label}
          </button>
        ))}
      </div>
      <div id={panelId(active)} role="tabpanel" aria-labelledby={tabId(active)}>
        {children}
      </div>
    </>
  );
}
