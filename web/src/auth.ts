import type { AuthResponse, AuthUser } from "./types";

/**
 * This tab's session, in memory only: the access token never touches localStorage or
 * sessionStorage (an XSS could read them). A reload restores it through the HttpOnly refresh
 * cookie (POST /auth/refresh, api.ts); other tabs hear of sign-ins, refreshes, and sign-outs over
 * the "uss-auth" BroadcastChannel.
 */
export type AuthSession = {
  /**
   * Who this tab acts as, stable across token refreshes: a new key means a new sign-in (or another
   * account adopted from another tab), so everything shown for the old one must go.
   */
  key: string;
  /** The short-lived access token (Bearer). */
  token: string;
  /** When the access token expires (ms since epoch): refreshed shortly before. */
  expiresAt: number;
  /** The absolute end of the sign-in (ms since epoch): no refresh extends past it. */
  sessionExpiresAt: number;
  user: AuthUser;
};

/** The keys an older build stored the session under; removed at startup, never written. */
export const LEGACY_AUTH_KEYS = ["uss.token", "uss.user"] as const;

/** Name of the Web Lock held around a refresh (one tab at a time spends the refresh cookie). */
export const REFRESH_LOCK = "uss-auth-refresh";
/** BroadcastChannel tabs tell each other about sessions on. */
export const AUTH_CHANNEL = "uss-auth";

/** What tabs say to each other on AUTH_CHANNEL (the session fields as the API sends them). */
export type AuthMessage = ({ type: "session" } & AuthResponse) | { type: "signing-out" } | { type: "signed-out" };

/**
 * sessionStorage flag (no credential in it) set while a sign-out is not yet confirmed by the
 * server: a reload of this tab then neither restores the session from the refresh cookie nor
 * shows a clean sign-out, but retries the logout.
 */
export const SIGN_OUT_PENDING_KEY = "uss.signout-pending";

/**
 * A sign-out the server has not confirmed yet. `local`: this tab sends POST /auth/logout (and
 * retries it); `elsewhere`: another tab does, this one only hides everything until it hears the
 * outcome. `token` is the access token to send as Bearer (memory only; null after a reload: the
 * refresh cookie alone is enough for the server).
 */
export type SignOutPending = { origin: "local" | "elsewhere"; token: string | null };

/** Why this tab's session ended: its own sign-out, the server refused it, or another tab signed out. */
export type SignOutReason = "local" | "expired" | "elsewhere";

export type SessionChange =
  /** Signed in here (login, or a restore at boot) or, while signed out, adopted from another tab. */
  | { kind: "signed-in"; session: AuthSession }
  /** The same account with a new access token or user record: nothing shown needs to go. */
  | { kind: "refreshed"; session: AuthSession }
  /** Another tab signed in as someone else: this tab now acts as that user. */
  | { kind: "switched"; session: AuthSession }
  /** A sign-out started (here or in another tab): nothing of the session may show, none is adopted. */
  | { kind: "signing-out"; origin: SignOutPending["origin"] }
  | { kind: "signed-out"; reason: SignOutReason };

/** Removes whatever an older build stored for the session (the token must not linger on disk). */
export function clearLegacyAuthStorage(): void {
  for (const storage of [safeStorage("localStorage"), safeStorage("sessionStorage")]) {
    if (!storage) continue;
    for (const key of LEGACY_AUTH_KEYS) {
      try {
        storage.removeItem(key);
      } catch {
        // Blocked storage: nothing could have been read from it either.
      }
    }
  }
}

function safeStorage(name: "localStorage" | "sessionStorage"): Storage | null {
  try {
    return typeof window === "undefined" ? null : window[name];
  } catch {
    return null;
  }
}

/** Same account and the same access (role, home dealership, customer link). */
export function sameAccess(a: AuthUser, b: AuthUser): boolean {
  return a.id === b.id && a.role === b.role && (a.dealershipId ?? "") === (b.dealershipId ?? "") && (a.customerId ?? "") === (b.customerId ?? "");
}

/**
 * Expiry (ms since epoch) of a JWT access token, read from its `exp` claim; null when the token
 * cannot be decoded. Only a fallback for a response without `expiresAt`: the server stays the judge.
 */
export function tokenExpiry(token: string): number | null {
  const part = token.split(".")[1];
  if (!part) return null;
  try {
    const json = atob(part.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(part.length / 4) * 4, "="));
    const exp = (JSON.parse(json) as { exp?: unknown }).exp;
    return typeof exp === "number" ? exp * 1000 : null;
  } catch {
    return null;
  }
}

/** The access token's lifetime assumed when neither `expiresAt` nor the token says (the server default). */
const FALLBACK_ACCESS_MS = 15 * 60_000;

type SessionFields = Omit<AuthSession, "key">;

function fieldsOf(r: Pick<AuthResponse, "token" | "user"> & { expiresAt?: string; sessionExpiresAt?: string }): SessionFields {
  const parsed = Date.parse(r.expiresAt ?? "");
  const expiresAt = Number.isFinite(parsed) ? parsed : (tokenExpiry(r.token) ?? Date.now() + FALLBACK_ACCESS_MS);
  const end = Date.parse(r.sessionExpiresAt ?? "");
  return { token: r.token, user: r.user, expiresAt, sessionExpiresAt: Number.isFinite(end) ? end : expiresAt };
}

let current: AuthSession | null = null;
let pending: SignOutPending | null = null;
let keySeq = 0;
const listeners = new Set<(change: SessionChange) => void>();
// undefined: not opened yet; null: this browser has no BroadcastChannel.
let channel: BroadcastChannel | null | undefined;

export function currentSession(): AuthSession | null {
  return current;
}

/** The access token this tab acts with (requests, the WebSocket ticket). */
export function tabSessionToken(): string | null {
  return current?.token ?? null;
}

/** Headers with this tab's token. */
export function authHeaders(extra?: HeadersInit, token: string | null = tabSessionToken()): HeadersInit {
  const headers = new Headers(extra);
  if (token) headers.set("Authorization", `Bearer ${token}`);
  return headers;
}

/** Listens for changes of this tab's session (and opens the cross-tab channel); returns the unsubscribe. */
export function onSessionChange(listener: (change: SessionChange) => void): () => void {
  openChannel();
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function emit(change: SessionChange) {
  for (const l of [...listeners]) l(change);
}

function openChannel(): BroadcastChannel | null {
  if (channel !== undefined) return channel;
  channel = typeof BroadcastChannel === "function" ? new BroadcastChannel(AUTH_CHANNEL) : null;
  channel?.addEventListener("message", (e: MessageEvent) => receive(e.data));
  return channel;
}

function post(message: AuthMessage) {
  try {
    openChannel()?.postMessage(message);
  } catch {
    // A closed channel (page unloading): the other tabs refresh on their own.
  }
}

function messageOf(s: AuthSession): AuthMessage {
  return {
    type: "session",
    token: s.token,
    expiresAt: new Date(s.expiresAt).toISOString(),
    sessionExpiresAt: new Date(s.sessionExpiresAt).toISOString(),
    user: s.user,
  };
}

function isUser(u: unknown): u is AuthUser {
  const v = u as Partial<AuthUser> | null;
  return typeof v === "object" && v !== null && typeof v.id === "string" && typeof v.name === "string" && typeof v.role === "number";
}

/** A message from another tab of this origin (still checked: a malformed one is ignored). */
function receive(data: unknown) {
  const msg = data as Partial<AuthMessage> | null;
  if (!msg || typeof msg !== "object") return;
  if (msg.type === "signed-out") {
    // Another tab's logout was confirmed: so is ours, if one was pending.
    const was = signOutPending();
    if (was) settle(was.origin === "local" ? "local" : "elsewhere", false);
    else if (current) end("elsewhere", false);
    return;
  }
  if (msg.type === "signing-out") {
    if (!signOutPending()) begin("elsewhere", false);
    return;
  }
  if (msg.type !== "session") return;
  // While a sign-out is unconfirmed nothing is adopted (a racing refresh elsewhere must not
  // bring the session back); a new login in another tab is picked up on a reload.
  if (signOutPending()) return;
  const m = msg as Partial<Extract<AuthMessage, { type: "session" }>>;
  if (typeof m.token !== "string" || !m.token || !isUser(m.user)) return;
  if (m.token === current?.token) return;
  adopt(fieldsOf({ token: m.token, user: m.user, expiresAt: m.expiresAt, sessionExpiresAt: m.sessionExpiresAt }), "remote");
}

/**
 * Makes `fields` this tab's session. A login always starts a new session here; a refresh or a
 * session heard from another tab keeps the key for the same account (with the same access) and
 * switches to a new one for anyone else. Everything but a remote session is told to the other tabs.
 */
function adopt(fields: SessionFields, source: "login" | "refresh" | "remote"): AuthSession {
  // A login replaces the session an unconfirmed sign-out was for (the server revokes the refresh
  // family the browser still carried).
  if (source === "login") setPending(null);
  const prev = current;
  const same = prev !== null && source !== "login" && sameAccess(prev.user, fields.user);
  const next: AuthSession = { ...fields, key: same ? prev.key : `s${++keySeq}` };
  current = next;
  if (source !== "remote") post(messageOf(next));
  emit(same ? { kind: "refreshed", session: next } : prev && source !== "login" ? { kind: "switched", session: next } : { kind: "signed-in", session: next });
  return next;
}

/** Starts the session a login or register answered with (and tells the other tabs). */
export function startSession(r: AuthResponse): AuthSession {
  return adopt(fieldsOf(r), "login");
}

/** Takes the session POST /auth/refresh answered with (and tells the other tabs). */
export function refreshedSession(r: AuthResponse): AuthSession {
  return adopt(fieldsOf(r), "refresh");
}

/** The server's current view of the user (api.me) for this tab's session `key`; kept in memory only. */
export function updateUser(key: string, user: AuthUser): void {
  if (!current || current.key !== key || current.user.id !== user.id) return;
  if (JSON.stringify(current.user) === JSON.stringify(user)) return;
  current = { ...current, user };
  emit({ kind: "refreshed", session: current });
}

function end(reason: SignOutReason, broadcast: boolean) {
  const had = current !== null;
  current = null;
  if (broadcast) post({ type: "signed-out" });
  if (had) emit({ kind: "signed-out", reason });
}

function sessionFlag(): Storage | null {
  return safeStorage("sessionStorage");
}

function setPending(next: SignOutPending | null) {
  pending = next;
  try {
    if (next) sessionFlag()?.setItem(SIGN_OUT_PENDING_KEY, "1");
    else sessionFlag()?.removeItem(SIGN_OUT_PENDING_KEY);
  } catch {
    // Blocked storage: the in-memory state still holds for this page.
  }
}

/** The unconfirmed sign-out of this tab, if any (also after a reload, from SIGN_OUT_PENDING_KEY). */
export function signOutPending(): SignOutPending | null {
  if (pending) return pending;
  try {
    if (sessionFlag()?.getItem(SIGN_OUT_PENDING_KEY)) pending = { origin: "local", token: null };
  } catch {
    // Blocked storage: nothing survived a reload either.
  }
  return pending;
}

function begin(origin: SignOutPending["origin"], broadcast: boolean) {
  const token = current?.token ?? pending?.token ?? null;
  current = null;
  setPending({ origin, token });
  if (broadcast) post({ type: "signing-out" });
  emit({ kind: "signing-out", origin });
}

function settle(reason: SignOutReason, broadcast: boolean) {
  setPending(null);
  if (broadcast) post({ type: "signed-out" });
  emit({ kind: "signed-out", reason });
}

/**
 * Starts this tab's sign-out: the session and everything shown go at once, the other tabs hide
 * theirs too ("signing-out"), and nothing is restored or adopted until the server confirms
 * (confirmSignOut). Also takes over a sign-out another tab started.
 */
export function beginSignOut(): void {
  begin("local", true);
}

/** The server confirmed the sign-out (204, or 401: nothing left to revoke): tell the other tabs. */
export function confirmSignOut(): void {
  if (!signOutPending()) return;
  settle("local", true);
}

/**
 * The server refused session `key` (a 401 after a refresh, a dead refresh cookie, or the session's
 * absolute end): it ends here, unless this tab moved on to another session since.
 */
export function signalAuthExpired(key: string | undefined = current?.key): void {
  if (current && key !== undefined && current.key === key) end("expired", false);
}

/** Runs `fn` holding the cross-tab refresh lock where the browser has Web Locks (else directly). */
export function withRefreshLock<T>(fn: () => Promise<T>): Promise<T> {
  const locks = typeof navigator !== "undefined" ? (navigator as Navigator & { locks?: LockManager }).locks : undefined;
  if (!locks || typeof locks.request !== "function") return fn();
  return locks.request(REFRESH_LOCK, () => fn()) as Promise<T>;
}

/** Test hook: forgets the session and closes the channel (a stubbed BroadcastChannel is reopened). */
export function resetAuthForTests(): void {
  current = null;
  pending = null;
  listeners.clear();
  channel?.close();
  channel = undefined;
}
