import { useState } from "react";

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

type State<T> = { scope: string; base: T; value: T; version: number };

/**
 * An editable draft of `source` (an admin form) that live updates do not wipe. `scope` names what
 * is edited (a dealership id): another scope starts a fresh draft. When `source` changes elsewhere
 * (another admin, a CONFIG_UPDATED reload) an untouched draft follows it; a dirty one is kept and
 * reports `stale`, so the form can say "changed elsewhere — reload" instead of losing the edits.
 * `version` is the one the draft is based on: send it as If-Match, so saving a stale draft gets a
 * 412 instead of overwriting the other change. A source that moved on in version only (a change
 * that does not touch these fields, e.g. a closure added next to the weekly hours) moves the base
 * version along. Only a newer version than the base counts as "changed elsewhere".
 */
export function useDraft<T>(source: T, version: number, scope: string) {
  const [draft, setDraft] = useState<State<T>>(() => ({ scope, base: source, value: source, version }));
  const dirty = !same(draft.value, draft.base);
  // A source older than the draft's base (the catalog not yet reloaded after this form's own
  // save, or the reload failed) is never followed: the saved state stays the base.
  const newer = version > draft.version;
  let current = draft;
  if (draft.scope !== scope || (newer && !dirty && !same(source, draft.base))) {
    current = { scope, base: source, value: source, version };
    setDraft(current);
  } else if (newer && same(source, draft.base)) {
    current = { ...draft, version };
    setDraft(current);
  }
  return {
    value: current.value,
    /** Edits the draft. */
    set: (update: (prev: T) => T) => setDraft((d) => ({ ...d, value: update(d.value) })),
    dirty: !same(current.value, current.base),
    /** Edited here and changed elsewhere since (a newer version with other values). */
    stale: version > current.version && !same(source, current.base),
    /** The version the draft is based on (If-Match). */
    version: current.version,
    /** Drops the edits and starts again from the latest source. */
    reload: () => setDraft({ scope, base: source, value: source, version }),
    /** After a save: the saved state is the new base. */
    rebase: (saved: T, savedVersion: number) => setDraft({ scope, base: saved, value: saved, version: savedVersion }),
  };
}
