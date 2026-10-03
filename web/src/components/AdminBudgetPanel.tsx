"use client";

// S-224 (S-203 WP2): admin Budget tab. Platform-wide budget knobs
// (default for new users / global max / optional platform monthly limit)
// plus a per-user budget editor with current-month spend. Talks to the
// WP1 endpoints only; the server enforces platform_admin, the page also
// hides the whole tab from non-admins (defence in depth).

import { useCallback, useEffect, useState } from "react";
import { apiGet, apiPut } from "../lib/api";
import { formatMoney } from "../lib/format";
import {
  budgetKnobsChanged,
  knobToInput,
  parseBudgetInput,
  type AdminBudgetsPayload,
  type CostBudgetStatus,
  type PlatformBudgetPutResponse,
} from "../lib/costs";

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

// ---------------------------------------------------------------------------
// Platform knobs
// ---------------------------------------------------------------------------

export function PlatformBudgetForm({
  payload,
  token,
  onSaved,
}: {
  payload: AdminBudgetsPayload;
  token: string;
  onSaved: (resp: PlatformBudgetPutResponse) => void;
}) {
  const [defaultMonthly, setDefaultMonthly] = useState(knobToInput(payload.platform.default_monthly_usd));
  const [max, setMax] = useState(knobToInput(payload.platform.max_usd));
  const [platformLimit, setPlatformLimit] = useState(knobToInput(payload.platform.platform_monthly_limit_usd));
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [saving, setSaving] = useState(false);

  const save = async () => {
    const parsed = {
      defaultMonthly: parseBudgetInput(defaultMonthly),
      max: parseBudgetInput(max),
      platformLimit: parseBudgetInput(platformLimit),
    };
    for (const res of [parsed.defaultMonthly, parsed.max, parsed.platformLimit]) {
      if (res.kind === "error") {
        setError(`Invalid value: ${res.message}`);
        return;
      }
    }
    const body = budgetKnobsChanged(payload.platform, parsed);
    if (Object.keys(body).length === 0) {
      setNotice("Nothing to save — no knob changed.");
      return;
    }
    setSaving(true);
    setError("");
    setNotice("");
    try {
      const resp = await apiPut<PlatformBudgetPutResponse>("/admin/budgets/platform", token, body);
      const clamped = resp.clamped_user_budgets ?? 0;
      setNotice(
        clamped > 0
          ? `Saved. ${clamped} user ${clamped === 1 ? "budget was" : "budgets were"} clamped down to the new max.`
          : "Saved.",
      );
      onSaved(resp);
    } catch (err) {
      setError(errorMessage(err, "Failed to save platform budgets"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="card" aria-label="Platform budget settings">
      <h2 className="section-title">Platform budgets</h2>
      <div className="field-row">
        <label className="field">
          <span>Default monthly budget (USD)</span>
          <input
            className="form-control"
            type="text"
            inputMode="decimal"
            placeholder="empty = no default"
            value={defaultMonthly}
            onChange={(e) => setDefaultMonthly(e.target.value)}
          />
          <span className="field-hint">Applied to users with no individual budget.</span>
        </label>
        <label className="field">
          <span>Global maximum budget (USD)</span>
          <input
            className="form-control"
            type="text"
            inputMode="decimal"
            placeholder="empty = no cap"
            value={max}
            onChange={(e) => setMax(e.target.value)}
          />
          <span className="field-hint">Lowering this clamps every user budget above the new max.</span>
        </label>
        <label className="field">
          <span>Platform-wide monthly limit (USD)</span>
          <input
            className="form-control"
            type="text"
            inputMode="decimal"
            placeholder="empty = no limit"
            value={platformLimit}
            onChange={(e) => setPlatformLimit(e.target.value)}
          />
          <span className="field-hint">Optional hard cap for the whole platform; clear for unlimited.</span>
        </label>
      </div>
      <div style={{ display: "flex", gap: "var(--space-3)", alignItems: "center", marginTop: "var(--space-2)" }}>
        <button type="button" className="btn btn-primary" onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save platform budgets"}
        </button>
        {notice ? <span className="field-hint">{notice}</span> : null}
      </div>
      {error ? <div className="notice error">{error}</div> : null}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Per-user editor row
// ---------------------------------------------------------------------------

function UserBudgetRow({
  user,
  maxUsd,
  token,
  onSaved,
}: {
  user: CostBudgetStatus;
  maxUsd: number | null;
  token: string;
  onSaved: (userId: string, budget: number) => void;
}) {
  const [value, setValue] = useState(knobToInput(user.monthly_budget_usd));
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const dirty = value !== knobToInput(user.monthly_budget_usd);

  const save = async () => {
    const parsed = parseBudgetInput(value);
    if (parsed.kind !== "value") {
      setError(parsed.kind === "clear" ? "A per-user budget must be a number (set the platform default for “no limit”)." : parsed.message);
      return;
    }
    if (maxUsd != null && parsed.value > maxUsd) {
      setError(`Exceeds the platform max (${formatMoney(maxUsd)}). Lower the max first — it clamps budgets instead.`);
      return;
    }
    setSaving(true);
    setError("");
    try {
      await apiPut(`/admin/budgets/users/${encodeURIComponent(user.user_id)}`, token, {
        monthly_budget_usd: parsed.value,
      });
      onSaved(user.user_id, parsed.value);
    } catch (err) {
      setError(errorMessage(err, "Failed to save budget"));
    } finally {
      setSaving(false);
    }
  };

  const overClass = user.over_budget ? "chip chip-over-budget" : undefined;
  return (
    <div className="entity-row budget-user-row">
      <div className="budget-user-id">
        <span className="entity-title">{user.name || user.email || user.user_id}</span>
        {user.name && user.email ? <span className="entity-meta">{user.email}</span> : null}
      </div>
      <div className="budget-user-spend">
        <span className="entity-meta">MTD spend</span>
        <strong>{formatMoney(user.mtd_cost)}</strong>
      </div>
      <div className="budget-user-state">
        {user.remaining_usd != null ? (
          <span className={overClass ?? "entity-meta"}>
            {user.over_budget
              ? `Over by ${formatMoney(user.mtd_cost - (user.monthly_budget_usd ?? 0))}`
              : `${formatMoney(user.remaining_usd)} left`}
          </span>
        ) : (
          <span className="entity-meta">No limit</span>
        )}
      </div>
      <div className="budget-user-edit">
        <input
          className="form-control"
          type="text"
          inputMode="decimal"
          aria-label={`Monthly budget for ${user.name || user.email || user.user_id}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        <button type="button" className="btn btn-sm btn-primary" onClick={save} disabled={!dirty || saving}>
          {saving ? "Saving…" : "Set"}
        </button>
      </div>
      {error ? <div className="notice error budget-user-error">{error}</div> : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Panel
// ---------------------------------------------------------------------------

export function AdminBudgetPanel({ token }: { token: string }) {
  const [data, setData] = useState<AdminBudgetsPayload | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      setData(await apiGet<AdminBudgetsPayload>("/admin/budgets", token));
      setError("");
    } catch (err) {
      setError(errorMessage(err, "Failed to load budgets"));
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    // Async fetch: setState lands in callbacks, not the effect body.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void load();
  }, [token, load]);

  const refreshAfterPlatformSave = useCallback(() => {
    void load();
  }, [load]);

  const refreshAfterUserSave = useCallback(() => {
    void load();
  }, [load]);

  if (loading && !data) {
    return <div className="entity-meta">Loading budgets…</div>;
  }
  if (!data) {
    return <div className="notice error">{error || "Budgets unavailable"}</div>;
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--space-4)" }}>
      {error ? <div className="notice error">{error}</div> : null}
      <PlatformBudgetForm payload={data} token={token} onSaved={refreshAfterPlatformSave} />
      <section aria-label="User budgets">
        <h2 className="section-title">
          User budgets{" "}
          <span className="entity-meta">
            (platform MTD: {formatMoney(data.platform.platform_mtd_cost)})
          </span>
        </h2>
        <div className="entity-list">
          {data.users.map((u) => (
            <UserBudgetRow key={u.user_id} user={u} maxUsd={data.platform.max_usd} token={token} onSaved={refreshAfterUserSave} />
          ))}
        </div>
      </section>
    </div>
  );
}
