"use client";

import { useState, type FormEvent, type ReactNode } from "react";
import { useAuth } from "../lib/auth";

// Gate every page on an authenticated principal.
// OIDC mode: the provider redirects to /auth/login automatically; this just
// shows the waiting state. Token mode (dev): paste-a-token gate.
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
    return (
      <div className="auth-gate">
        <div className="notice">Redirecting to sign-in…</div>
      </div>
    );
  }

  return (
    <div className="auth-gate">
      <div className="card auth-card">
        <h1 style={{ margin: "0 0 var(--space-2)", fontSize: "var(--text-xl)" }}>Sign in to Skquad v2</h1>
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
