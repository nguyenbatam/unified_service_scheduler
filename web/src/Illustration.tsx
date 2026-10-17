import type { ReactNode } from "react";

// Spot illustrations for empty states and the sign-in page, drawn on a 160×100 grid. Colours come
// from the ill-* classes in styles.css (CSS variables do not work in SVG presentation attributes).
// No <text>: an illustration adds nothing to the textContent or the name of what contains it.

const shadow = <ellipse className="ill-shadow" cx="80" cy="93" rx="58" ry="5" />;

/** A calendar page with a 3×3 day grid; `children` draws over it. */
function CalendarPage({ children }: { children?: ReactNode }) {
  return (
    <>
      <rect className="ill-paper" x="40" y="18" width="76" height="68" rx="9" />
      <path className="ill-accent" d="M40 27a9 9 0 0 1 9-9h58a9 9 0 0 1 9 9v9H40z" />
      <path className="ill-ink" d="M60 11v14M96 11v14" strokeWidth="5" strokeLinecap="round" />
      {[0, 1, 2].flatMap((row) =>
        [0, 1, 2].map((col) => (
          <rect key={`${row}-${col}`} className="ill-soft" x={50 + col * 20} y={44 + row * 13} width="15" height="9" rx="3" />
        )),
      )}
      {children}
    </>
  );
}

const ART = {
  /** Nothing booked yet: a calendar with a "+" badge. */
  calendar: (
    <CalendarPage>
      <circle className="ill-accent" cx="116" cy="78" r="15" />
      <path className="ill-on-accent" d="M116 71v14M109 78h14" strokeWidth="3.5" strokeLinecap="round" />
    </CalendarPage>
  ),
  /** No free time found: a calendar with a "×" badge. */
  noSlots: (
    <CalendarPage>
      <circle className="ill-warn" cx="116" cy="78" r="15" />
      <path className="ill-on-accent" d="m110 72 12 12M122 72l-12 12" strokeWidth="3.5" strokeLinecap="round" />
    </CalendarPage>
  ),
  /** Not searched yet: a grid of start times under a magnifier. */
  search: (
    <>
      {[0, 1].flatMap((row) =>
        [0, 1, 2].map((col) => (
          <rect
            key={`${row}-${col}`}
            className={row === 0 && col === 1 ? "ill-accent" : "ill-soft"}
            x={26 + col * 32}
            y={30 + row * 22}
            width="27"
            height="16"
            rx="5"
          />
        )),
      )}
      <circle className="ill-glass" cx="108" cy="48" r="19" />
      <path className="ill-ink" d="m122 62 14 14" strokeWidth="7" strokeLinecap="round" />
    </>
  ),
  /** Dealership closed that day: a shop with a padlock on the door. */
  closed: (
    <>
      <rect className="ill-paper" x="36" y="36" width="88" height="52" rx="4" />
      <path className="ill-accent" d="M30 38l8-20h84l8 20z" />
      <path className="ill-on-accent" d="M58 18l-4 20M80 18v20M102 18l4 20" strokeWidth="2" />
      <rect className="ill-soft" x="66" y="52" width="28" height="36" rx="3" />
      <rect className="ill-warn" x="71" y="66" width="18" height="14" rx="3" />
      <path className="ill-ink" d="M75 66v-4a5 5 0 0 1 10 0v4" strokeWidth="3" fill="none" />
    </>
  ),
  /** Nobody matched the search: a person with a question mark. */
  person: (
    <>
      <circle className="ill-soft" cx="70" cy="38" r="15" />
      <path className="ill-soft" d="M42 88c0-17 12-28 28-28s28 11 28 28z" />
      <circle className="ill-accent" cx="112" cy="40" r="17" />
      <path className="ill-on-accent" d="M106 35a6 6 0 1 1 8 5.6c-1.3.5-2 1.6-2 3V45" strokeWidth="3.5" strokeLinecap="round" fill="none" />
      <circle className="ill-on-accent-fill" cx="112" cy="51" r="2.2" />
    </>
  ),
  /** Nothing failed: a big tick with sparkles. */
  allClear: (
    <>
      <circle className="ill-soft" cx="80" cy="50" r="34" />
      <circle className="ill-accent" cx="80" cy="50" r="23" />
      <path className="ill-on-accent" d="m69 50 8 8 15-15" strokeWidth="5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
      <path className="ill-ink" d="M124 22v10M119 27h10M36 66v8M32 70h8M128 70v6M125 73h6" strokeWidth="2.5" strokeLinecap="round" />
    </>
  ),
  /** The workshop: a service bay with a car in it (sign-in page, first car, a dealership without bays). */
  garage: (
    <>
      <path className="ill-paper" d="M30 40 80 16l50 24v48H30z" />
      <path className="ill-ink" d="M24 42 80 15l56 27" strokeWidth="5" strokeLinecap="round" strokeLinejoin="round" fill="none" />
      <rect className="ill-soft" x="46" y="48" width="68" height="40" rx="3" />
      <path className="ill-line" d="M46 53h68M46 58h68" strokeWidth="2" />
      <path className="ill-accent" d="M58 74l6-10h28l7 10h4a4 4 0 0 1 4 4v6H52v-6a4 4 0 0 1 4-4z" />
      <path className="ill-glass-fill" d="M66 66h11v8H61zM80 66h11l5 8H80z" />
      <circle className="ill-ink-fill" cx="66" cy="85" r="5" />
      <circle className="ill-ink-fill" cx="94" cy="85" r="5" />
    </>
  ),
} satisfies Record<string, ReactNode>;

export type IllustrationName = keyof typeof ART;

/** A decorative spot illustration (aria-hidden): the text next to it carries the meaning. */
export function Illustration({ name, className }: { name: IllustrationName; className?: string }) {
  return (
    <svg
      className={className ? `illustration ${className}` : "illustration"}
      viewBox="0 0 160 100"
      aria-hidden="true"
      focusable="false"
    >
      {shadow}
      {ART[name]}
    </svg>
  );
}

/** An empty or waiting state: an illustration above its explanation (and optional actions). */
export function EmptyState({ art, children }: { art: IllustrationName; children: ReactNode }) {
  return (
    <div className="empty-state">
      <Illustration name={art} />
      <p className="hint">{children}</p>
    </div>
  );
}
