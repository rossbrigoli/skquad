// Skquad UI v2 — machine-value formatting. All costs, token counts, times and
// lease ages render through these helpers; never format ad hoc per screen.

import type { Message, MeteringSummary, Task } from "./api";

export function formatCost(summary: MeteringSummary | null): string {
  if (!summary) {
    return "-";
  }
  return formatMoney(summary.cost ?? 0, normalizedCurrency(summary.currency));
}

// formatMoney renders a currency amount. Four decimals cover ordinary spend,
// but per-1M-token LLM pricing routinely produces sub-tenth-of-cent totals
// that `toFixed(4)` would show as a misleading "0.0000" — for those we show
// two significant digits instead, so real spend never looks like no spend.
export function formatMoney(amount: number, currency = "USD"): string {
  const abs = Math.abs(amount);
  if (abs === 0) {
    return `${currency} 0.0000`;
  }
  if (abs >= 0.0001) {
    return `${currency} ${amount.toFixed(4)}`;
  }
  return `${currency} ${trimZeros(amount.toPrecision(2))}`;
}

function normalizedCurrency(currency?: string): string {
  const trimmed = currency?.trim() ?? "";
  return trimmed === "" ? "USD" : trimmed;
}

function trimZeros(value: string): string {
  if (!value.includes(".")) {
    return value;
  }
  let end = value.length;
  while (end > 0 && value[end - 1] === "0") {
    end -= 1;
  }
  const trimmed = value.slice(0, end);
  return trimmed.endsWith(".") ? `${trimmed}0` : trimmed;
}

export function formatTokens(summary: MeteringSummary | null): string {
  if (!summary) {
    return "no usage recorded";
  }
  const input = summary.input_tokens ?? 0;
  const output = summary.output_tokens ?? 0;
  if (input === 0 && output === 0) {
    return "no usage recorded";
  }
  return `${compact(input)} in · ${compact(output)} out`;
}

function compact(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

export type LeaseState = "running" | "stalled" | "idle";

// A task holds a lease while an agent runtime is actively working it. An
// expired lease means the worker stopped heartbeating without completing.
export function leaseState(task: Task): LeaseState {
  if (!task.execution_id || !task.lease_expires_at) {
    return "idle";
  }
  const expiry = Date.parse(task.lease_expires_at);
  // Go serialises a zero time.Time as 0001-01-01T00:00:00Z rather than omitting
  // it, and that string is truthy but parses to a large negative timestamp.
  // Anything before the epoch means "no lease", not "lease expired long ago".
  if (Number.isNaN(expiry) || expiry <= 0) {
    return "idle";
  }
  return expiry > Date.now() ? "running" : "stalled";
}

export function formatRelativeTime(value?: string): string {
  if (!value) {
    return "";
  }
  const timestamp = Date.parse(value);
  if (Number.isNaN(timestamp)) {
    return "";
  }
  const deltaSec = Math.round((timestamp - Date.now()) / 1000);
  const absSec = Math.abs(deltaSec);
  const [amount, unit]: [number, Intl.RelativeTimeFormatUnit] =
    absSec < 60 ? [deltaSec, "second"]
    : absSec < 3600 ? [Math.round(deltaSec / 60), "minute"]
    : absSec < 86400 ? [Math.round(deltaSec / 3600), "hour"]
    : [Math.round(deltaSec / 86400), "day"];
  return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(amount, unit);
}

export function messageText(message: Message): string {
  const payload = message.payload || {};
  if (typeof payload.message === "string") {
    return payload.message;
  }
  return JSON.stringify(payload);
}

export function messageDeliveryNote(message: Message): string {
  const parts: string[] = [];
  if (typeof message.attempts === "number" && message.attempts > 0) {
    const max = message.max_attempts ? `/${message.max_attempts}` : "";
    parts.push(`attempt ${message.attempts}${max}`);
  }
  if (message.next_retry_at) {
    const due = Date.parse(message.next_retry_at);
    parts.push(
      !Number.isNaN(due) && due <= Date.now() ? "retry due" : `retry ${formatRelativeTime(message.next_retry_at)}`,
    );
  }
  if (message.expires_at) {
    parts.push(`expires ${formatRelativeTime(message.expires_at)}`);
  }
  return parts.join(" · ");
}
