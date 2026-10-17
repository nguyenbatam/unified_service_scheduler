import { useEffect, useRef, useState } from "react";
import { api, refreshSession } from "./api";
import {
  beginSignOut,
  clearLegacyAuthStorage,
  confirmSignOut,
  currentSession,
  onSessionChange,
  sameAccess,
  signOutPending,
  signalAuthExpired,
  startSession,
  updateUser,
  type AuthSession,
  type SessionChange,
  type SignOutReason,
} from "./auth";
import type { AuthResponse, AuthUser } from "./types";

export { sameAccess };

export const SESSION_EXPIRED = "Your session has expired — please sign in again.";
export const SIGNED_OUT_ELSEWHERE = "You signed out in another tab.";
export const SESSION_CHECK_FAILED = "Could not check your session — the server did not answer. Please sign in.";
/** The warning before the sign-in's absolute end comes this long ahead. */
export const EXPIRY_WARNING_MS = 5 * 60_000;
/** The access token is refreshed this long before it expires. */
export const REFRESH_AHEAD_MS = 60_000;
/**
 * Refreshes are at least this far apart: with a token that is (by this browser's clock) already
 * expired or about to — a skewed clock, a 1-minute ACCESS_TOKEN_TTL — this is no busy loop; a
 * request that meets an expired token refreshes on its 401 anyway (api.ts).
 */
export const MIN_REFRESH_GAP_MS = 10_000;
/** A refresh that failed for a transient reason (network, 429, 5xx) is tried again after this. */
export const REFRESH_RETRY_MS = 15_000;
/**
 * When the logout attempts of one round start, from the first (ms after it): a sign-out the
 * server did not confirm is retried a few times, then waits for "Try again" or the `online` event.
 */
export const SIGN_OUT_RETRY_DELAYS_MS = [0, 1_000, 3_000, 7_000];

/**
 * An unconfirmed sign-out: `signing-out` (first attempt out), `retrying` (an attempt failed, more
 * are scheduled), `failed` (the round is over: the user is asked to retry or close the browser),
 * `elsewhere` (another tab is signing out; this one only hides everything).
 */
export type SignOutStatus = "signing-out" | "retrying" | "failed" | "elsewhere";

/** setTimeout's longest delay (about 24.8 days). */
const MAX_TIMEOUT_MS = 2_147_483_647;

/** Said when another tab signed in as someone else: this tab now acts as that user. */
export function signedInElsewhere(name: string | undefined): string {
  return `You signed in${name ? ` as ${name}` : " again"} in another tab — this tab switched to that session and its view was reset.`;
}

const SIGN_OUT_NOTICE: Record<SignOutReason, string> = {
  local: "",
  expired: SESSION_EXPIRED,
  elsewhere: SIGNED_OUT_ELSEWHERE,
};

/** Runs `fn` once the clock reaches `when` (ms), re-arming past setTimeout's limit; returns the cancel. */
function at(when: number, fn: () => void): () => void {
  let timer: number;
  const arm = () => {
    const ms = when - Date.now();
    if (ms <= 0) fn();
    else timer = window.setTimeout(arm, Math.min(ms, MAX_TIMEOUT_MS));
  };
  timer = window.setTimeout(arm, Math.min(Math.max(0, when - Date.now()), MAX_TIMEOUT_MS));
  return () => window.clearTimeout(timer);
}

/**
 * The signed-in session of this tab, held in memory only (auth.ts). At mount it is restored from
 * the HttpOnly refresh cookie (POST /auth/refresh: `checking` until it answers); the access token
 * is refreshed REFRESH_AHEAD_MS before it expires (a 401 refreshes too, api.ts); the session ends
 * on sign-out, when the server refuses it, or at its absolute end (`sessionExpiresAt`, warned
 * EXPIRY_WARNING_MS ahead). Other tabs' sign-ins and refreshes arrive over BroadcastChannel: the
 * same account is adopted silently; another account drops everything shown with a notice;
 * another tab's sign-out signs this one out. `onSignOut` clears whatever the app holds for the
 * user (it always sees the latest closure); it runs whenever the session ends or changes account.
 */
export function useSession(onSignOut: () => void) {
  const [session, setSession] = useState<AuthSession | null>(() => {
    clearLegacyAuthStorage();
    return currentSession();
  });
  const [checking, setChecking] = useState(() => currentSession() === null && !signOutPending());
  const [signOut, setSignOut] = useState<SignOutStatus | null>(() => {
    const p = signOutPending();
    return p ? (p.origin === "local" ? "signing-out" : "elsewhere") : null;
  });
  // Bumped by "Try again": starts a new round of logout attempts.
  const [signOutRound, setSignOutRound] = useState(0);
  const [notice, setNotice] = useState("");
  // The sign-in's absolute end (ms) once it is less than EXPIRY_WARNING_MS away; null otherwise.
  const [expiresAt, setExpiresAt] = useState<number | null>(null);
  const latest = useRef(onSignOut);
  latest.current = onSignOut;

  useEffect(() => {
    const apply = (change: SessionChange) => {
      if (change.kind === "signing-out") {
        latest.current();
        setSession(null);
        setNotice("");
        setExpiresAt(null);
        setSignOut(change.origin === "local" ? "signing-out" : "elsewhere");
      } else if (change.kind === "signed-out") {
        latest.current();
        setSession(null);
        setNotice(SIGN_OUT_NOTICE[change.reason]);
        setExpiresAt(null);
        setSignOut(null);
      } else {
        if (change.kind === "signed-in") setSignOut(null);
        if (change.kind === "switched") {
          latest.current();
          setNotice(signedInElsewhere(change.session.user.name));
          setExpiresAt(null);
        } else if (change.kind === "signed-in") {
          setNotice("");
          setExpiresAt(null);
        }
        setSession(change.session);
      }
      setChecking(false);
    };
    const off = onSessionChange(apply);
    // A change between the first render and this subscription.
    setSession(currentSession());
    return off;
  }, []);

  // Boot: restore the session from the refresh cookie (StrictMode's second mount shares the
  // in-flight request). Already signed in (another mount, or a test) needs no request.
  useEffect(() => {
    // An unconfirmed sign-out (also from before a reload) never signs back in from the cookie.
    if (currentSession() || signOutPending()) {
      setChecking(false);
      return;
    }
    let cancelled = false;
    refreshSession()
      .catch(() => {
        if (!cancelled && !currentSession()) setNotice(SESSION_CHECK_FAILED);
      })
      .finally(() => {
        if (!cancelled) setChecking(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Proactive refresh shortly before the access token expires (keyed on the token: every new one
  // reschedules it); a transient failure tries again until the session's end, a refused one
  // already signed out (refreshSession).
  useEffect(() => {
    if (!session) return;
    const end = session.sessionExpiresAt;
    let cancel: () => void;
    const run = () => {
      refreshSession().catch(() => {
        if (Date.now() < end) cancel = at(Date.now() + REFRESH_RETRY_MS, run);
      });
    };
    // REFRESH_AHEAD_MS early, or halfway through a lifetime shorter than twice that.
    const lead = Math.max(0, Math.min(REFRESH_AHEAD_MS, (session.expiresAt - Date.now()) / 2));
    cancel = at(Math.max(session.expiresAt - lead, Date.now() + MIN_REFRESH_GAP_MS), run);
    return () => cancel();
  }, [session?.token]);

  // This tab's unconfirmed sign-out: POST /auth/logout until the server confirms it (204/401),
  // in rounds of SIGN_OUT_RETRY_DELAYS_MS; coming back online starts a new round.
  const signingOutHere = signOut !== null && signOut !== "elsewhere";
  useEffect(() => {
    if (!signingOutHere) return;
    const ctl = new AbortController();
    let timer: number | undefined;
    let attempt = 0;
    let inFlight = false;
    const run = async () => {
      timer = undefined;
      if (inFlight || ctl.signal.aborted) return;
      inFlight = true;
      const outcome = await api.logout(signOutPending()?.token ?? null, ctl.signal);
      inFlight = false;
      if (ctl.signal.aborted) return;
      if (outcome === "confirmed") {
        confirmSignOut();
        return;
      }
      attempt++;
      if (attempt < SIGN_OUT_RETRY_DELAYS_MS.length) {
        setSignOut("retrying");
        timer = window.setTimeout(run, SIGN_OUT_RETRY_DELAYS_MS[attempt] - SIGN_OUT_RETRY_DELAYS_MS[attempt - 1]);
      } else setSignOut("failed");
    };
    const onOnline = () => {
      if (inFlight) return;
      if (timer !== undefined) window.clearTimeout(timer);
      attempt = 0;
      void run();
    };
    window.addEventListener("online", onOnline);
    void run();
    return () => {
      ctl.abort();
      if (timer !== undefined) window.clearTimeout(timer);
      window.removeEventListener("online", onOnline);
    };
  }, [signingOutHere, signOutRound]);

  // The sign-in's absolute end: a warning EXPIRY_WARNING_MS before, then sign-out.
  useEffect(() => {
    if (!session) return;
    const { key, sessionExpiresAt: end } = session;
    const ms = end - Date.now();
    if (ms <= 0) {
      signalAuthExpired(key);
      return;
    }
    const cancels: (() => void)[] = [];
    if (ms <= EXPIRY_WARNING_MS) setExpiresAt(end);
    else {
      setExpiresAt(null);
      cancels.push(at(end - EXPIRY_WARNING_MS, () => setExpiresAt(end)));
    }
    cancels.push(at(end, () => signalAuthExpired(key)));
    return () => cancels.forEach((c) => c());
  }, [session?.key, session?.sessionExpiresAt]);

  return {
    session,
    /** True until the boot refresh answered: show neither the app nor the login form yet. */
    checking,
    notice,
    dismissNotice: () => setNotice(""),
    /** When the sign-in ends (ms), once that is less than EXPIRY_WARNING_MS away; else null. */
    expiresAt,
    dismissExpiry: () => setExpiresAt(null),
    /** A login or register answered: start the session here and in the other tabs. */
    signIn: (r: AuthResponse) => void startSession(r),
    /**
     * Ends session `key` here as refused by the server (the expiry notice; no server call, no
     * broadcast); a key this tab no longer uses is ignored.
     */
    expire: (key = currentSession()?.key) => signalAuthExpired(key),
    /**
     * Signs out everywhere: the session and the view go at once (here and in the other tabs), but
     * the sign-out only counts once the server confirmed it (`signOut` says how it is going).
     */
    logout: () => beginSignOut(),
    /** An unconfirmed sign-out, or null. */
    signOut: signOut,
    /** Sends the logout again now (from this tab, also when another tab had started it). */
    retrySignOut: () => {
      if (signOutPending()?.origin !== "local") beginSignOut();
      setSignOut("signing-out");
      setSignOutRound((n) => n + 1);
    },
    /** The server's current view of the user (api.me), for the session it was read for. */
    setUser: (user: AuthUser, key = currentSession()?.key) => {
      if (key) updateUser(key, user);
    },
  };
}
