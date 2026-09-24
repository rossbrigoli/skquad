"use client";

import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useAuth } from "../lib/auth";

// Gate every page on an authenticated principal.
//
// OIDC mode shows a chooser rather than auto-redirecting: the normal SSO path,
// plus a break-glass admin form when the control-plane advertises it is enabled.
// Token mode (dev) keeps the paste-a-token gate.
export function AuthGate({ children }: { children: ReactNode }) {
  const { token, user, loading, error, mode, setToken } = useAuth();
  const [draft, setDraft] = useState("");

  const authed = mode === "oidc" ? !!user : !!token && !error;

  if (loading) {
    return (
      <div className="auth-gate">
        <div className="notice">Checking session…</div>
      </div>
    );
  }

  if (authed) {
    return <>{children}</>;
  }

  if (mode === "oidc") {
    return <LoginChooser />;
  }

  return (
    <div className="auth-gate">
      <div className="card auth-card">
        <h1 style={{ margin: "0 0 var(--space-2)", fontSize: "var(--text-xl)" }}>Sign in to Skquad</h1>
        <p style={{ color: "var(--ink-muted)", marginTop: 0 }}>
          Paste your Skquad API token. The v2 UI talks to the same control-plane API as v1.
        </p>
        <form
          onSubmit={(event: FormEvent) => {
            event.preventDefault();
            setToken(draft.trim());
          }}
          style={{ display: "flex", gap: "var(--space-2)" }}
        >
          <input
            type="password"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="skquad API token"
            style={{ flex: 1 }}
            autoFocus
          />
          <button type="submit" className="btn btn-primary">
            Sign in
          </button>
        </form>
        {error ? <div className="notice error" style={{ marginTop: "var(--space-3)" }}>{error}</div> : null}
      </div>
    </div>
  );
}

function LoginChooser() {
  const [breakGlass, setBreakGlass] = useState(false);
  const [showForm, setShowForm] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [bgError, setBgError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    fetch("/auth/breakglass/status", { cache: "no-store" })
      .then((r) => (r.ok ? r.json() : { enabled: false }))
      .then((b: { enabled?: boolean }) => {
        if (!cancelled) setBreakGlass(b.enabled === true);
      })
      .catch(() => setBreakGlass(false));
    return () => {
      cancelled = true;
    };
  }, []);

  const submit = useCallback(
    async (event: FormEvent) => {
      event.preventDefault();
      setBusy(true);
      setBgError(null);
      try {
        const res = await fetch("/auth/breakglass", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ username, password }),
        });
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        if (!res.ok) {
          setBgError(body.error || "break-glass login failed");
          setBusy(false);
          return;
        }
        // The session cookie is set; reload so the app picks it up.
        window.location.reload();
      } catch {
        setBgError("Could not reach the server.");
        setBusy(false);
      }
    },
    [username, password],
  );

  return (
    <div className="auth-gate">
      <div className="card auth-card">
        <h1 style={{ margin: "0 0 var(--space-2)", fontSize: "var(--text-xl)" }}>Sign in to Skquad</h1>
        <p style={{ color: "var(--ink-muted)", marginTop: 0 }}>
          Choose how you want to authenticate.
        </p>

        <button
          type="button"
          className="btn btn-primary"
          style={{ width: "100%", justifyContent: "center", marginTop: "var(--space-3)" }}
          onClick={() => {
            window.location.href = "/auth/login";
          }}
        >
          Continue with GitHub (SSO)
        </button>

        {breakGlass ? (
          <div style={{ marginTop: "var(--space-4)", borderTop: "1px solid var(--line)", paddingTop: "var(--space-3)" }}>
            {!showForm ? (
              <button
                type="button"
                className="btn"
                style={{ width: "100%", justifyContent: "center" }}
                onClick={() => setShowForm(true)}
              >
                Break-glass admin
              </button>
            ) : (
              <form onSubmit={submit} style={{ display: "grid", gap: "var(--space-2)" }}>
                <div className="notice" style={{ marginBottom: 0 }}>
                  Break-glass bypasses SSO and grants <strong>platform_admin</strong>. It is only
                  reachable from the LAN or Tailscale — never through the public hostname.
                </div>
                <label className="field" style={{ display: "grid", gap: "var(--space-1)" }}>
                  <span>Username</span>
                  <input
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                    autoComplete="username"
                    autoFocus
                  />
                </label>
                <label className="field" style={{ display: "grid", gap: "var(--space-1)" }}>
                  <span>Password</span>
                  <input
                    type="password"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    autoComplete="current-password"
                  />
                </label>
                <div style={{ display: "flex", gap: "var(--space-2)" }}>
                  <button type="submit" className="btn btn-primary" disabled={busy || !username || !password}>
                    {busy ? "Signing in…" : "Sign in as break-glass"}
                  </button>
                  <button
                    type="button"
                    className="btn"
                    onClick={() => {
                      setShowForm(false);
                      setBgError(null);
                    }}
                  >
                    Cancel
                  </button>
                </div>
                {bgError ? <div className="notice error" style={{ marginTop: 0 }}>{bgError}</div> : null}
              </form>
            )}
          </div>
        ) : null}
      </div>
    </div>
  );
}
