import { type FormEvent, useState } from "react";
import { ErrorBanner } from "./ErrorBanner";
import { api } from "./api";
import type { AuthResponse } from "./types";
import type { SignOutStatus } from "./useSession";
import { useAsyncAction } from "./useAsyncAction";
import { Icon, type IconName } from "./Icon";
import { Illustration } from "./Illustration";

export type DemoAccount = { label: string; detail: string; email: string; password: string; icon?: IconName };

type Props = {
  /** The login or register answer (the refresh cookie came with it as Set-Cookie). */
  onAuthed: (auth: AuthResponse) => void;
  /** Why the user is here again (e.g. the session expired). */
  notice?: string;
  /** One-click sign-in buttons; none when empty (default: DEMO_ACCOUNTS). */
  demoAccounts?: readonly DemoAccount[];
  /** A sign-out the server has not confirmed yet (useSession), with its retry. */
  signOut?: SignOutStatus | null;
  onRetrySignOut?: () => void;
};

const SIGN_OUT_TEXT: Record<SignOutStatus, string> = {
  "signing-out": "Signing out…",
  retrying: "Sign-out not confirmed by the server yet — retrying…",
  failed:
    "Could not sign out: the server did not confirm it, so this browser may still be signed in. Try again, or close the browser before leaving this computer.",
  elsewhere: "Signing out in another tab…",
};

/**
 * The accounts the backend seeds for demos (backend/internal/seed); one click signs in. Shipped only
 * in a demo build — the dev server, or `VITE_DEMO=true` at build time (the compose stack's web
 * image, see web/Dockerfile) — never in a plain production build: the condition is static, so the
 * bundler drops the accounts and their passwords from it.
 */
export const DEMO_ACCOUNTS: readonly DemoAccount[] =
  import.meta.env.DEV || import.meta.env.VITE_DEMO === "true"
    ? [
        { label: "Group admin", detail: "every dealership + configuration", email: "admin@scheduler.local", password: "admin123", icon: "shield" },
        { label: "Dispatcher", detail: "Northside workshop", email: "dispatcher@scheduler.local", password: "dispatch123", icon: "headset" },
        { label: "Customer", detail: "Amelia Clarke", email: "customer@scheduler.local", password: "customer123", icon: "user" },
      ]
    : [];

export function AuthPanel({ onAuthed, notice, demoAccounts = DEMO_ACCOUNTS, signOut = null, onRetrySignOut }: Props) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const { busy, error, setError, run } = useAsyncAction();

  async function signIn(credentials: { email: string; password: string }) {
    await run(async () => {
      onAuthed(await api.login(credentials));
    });
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (mode === "login") {
      await signIn({ email, password });
      return;
    }
    await run(async () => {
      onAuthed(await api.register({ email, password, name }));
    });
  }

  function switchMode(next: "login" | "register") {
    // The email carries over (a user who meant to sign in, or the reverse); the password never does.
    setMode(next);
    setPassword("");
    setError("");
  }

  return (
    <main className="page auth-page">
      <header className="hero">
        <Illustration name="garage" className="hero-art" />
        <p className="eyebrow">Keyloop Scenario A</p>
        <h1>
          <Icon name="wrench" />
          Unified Service Scheduler
        </h1>
        <p className="lede">Sign in to book and manage appointments.</p>
      </header>

      <section className="panel auth-panel">
        <div className="auth-tabs">
          <button
            type="button"
            className={mode === "login" ? "active" : ""}
            onClick={() => switchMode("login")}
          >
            <Icon name="logIn" />
            Login
          </button>
          <button
            type="button"
            className={mode === "register" ? "active" : ""}
            onClick={() => switchMode("register")}
          >
            <Icon name="userPlus" />
            Register
          </button>
        </div>

        {signOut ? (
          <div className={`banner ${signOut === "failed" ? "warn" : "info"}`} role={signOut === "failed" ? "alert" : "status"}>
            <span>
              <Icon name={signOut === "failed" ? "alert" : "clock"} />
              {SIGN_OUT_TEXT[signOut]}
            </span>
            {onRetrySignOut && (signOut === "failed" || signOut === "elsewhere") ? (
              <button type="button" onClick={onRetrySignOut}>
                <Icon name={signOut === "failed" ? "refresh" : "logOut"} />
                {signOut === "failed" ? "Try again" : "Sign out here"}
              </button>
            ) : null}
          </div>
        ) : null}
        {notice && !error ? (
          <p className="banner info" role="status">
            <Icon name="info" />
            {notice}
          </p>
        ) : null}
        <ErrorBanner message={error} />

        <form className="auth-form" onSubmit={submit}>
          {mode === "register" ? (
            <label>
              <span>
                <Icon name="user" />
                Name
              </span>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
                autoComplete="name"
              />
            </label>
          ) : null}
          <label>
            <span>
              <Icon name="mail" />
              Email
            </span>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
              autoComplete="username"
            />
          </label>
          <label>
            <span>
              <Icon name="lock" />
              Password
            </span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              minLength={8}
              autoComplete={mode === "login" ? "current-password" : "new-password"}
            />
          </label>
          <button type="submit" className="primary" disabled={busy}>
            <Icon name={mode === "login" ? "logIn" : "userPlus"} />
            {mode === "login" ? "Sign in" : "Create account"}
          </button>
        </form>

        {mode === "login" ? (
          demoAccounts.length > 0 ? (
            <div className="demo-accounts">
              <p className="hint">Or try a demo account:</p>
              <div className="actions">
                {demoAccounts.map((a) => (
                  <button
                    key={a.email}
                    type="button"
                    disabled={busy}
                    title={`${a.email} / ${a.password}`}
                    onClick={() => void signIn({ email: a.email, password: a.password })}
                  >
                    <Icon name={a.icon ?? "user"} />
                    {a.label} <small className="hint">· {a.detail}</small>
                  </button>
                ))}
              </div>
            </div>
          ) : null
        ) : (
          <p className="hint">Creates a customer account for booking your own car; password 8+ characters.</p>
        )}
      </section>
    </main>
  );
}
