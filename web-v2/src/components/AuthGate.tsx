"use client";

import { useState, type FormEvent, type ReactNode } from "react";
import { useAuth } from "../lib/auth";

// Gate every page on a token. v1 uses the same bearer-token model; the token
// comes from the Skquad API (user provisioned).
export function AuthGate({ children }: { children: ReactNode }) {
  const { token, loading, error, setToken } = useAuth();
  const [draft, setDraft] = useState("");

  if (loading && token) {
    return (
      <div className="auth-gate">
        <div className="notice">Checking session…</div>
      </div>
    );
  }

  if (token && !error) {
    return <>{children}</>;
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
            placeholder="API token"
            style={{
              flex: 1,
              padding: "var(--space-2) var(--space-3)",
              borderRadius: "var(--radius-md)",
              border: "1px solid var(--line-strong)",
              background: "var(--surface)",
              color: "var(--ink)",
            }}
          />
          <button type="submit" className="btn btn-primary" disabled={!draft.trim()}>
            Sign in
          </button>
        </form>
        {error ? <div className="notice error" style={{ marginTop: "var(--space-3)" }}>{error}</div> : null}
      </div>
    </div>
  );
}
