import type { Activity, ContextUsage } from "./types";

// Use the newest request snapshot, never the cumulative input/output bill.
// Results/usage carry the snapshot too, so pagination can omit its original row.
export function latestContextUsage(activities: Activity[]): ContextUsage | null {
  let latest: ContextUsage | null = null;
  let seq = -Infinity;
  for (const step of activities) {
    if (step.seq < seq) continue;
    if (step.kind === "llm_switch" || step.kind === "llm_failover") {
      latest = null;
      seq = step.seq;
      continue;
    }
    const usage = step.metadata?.context_usage;
    if (
      !usage ||
      !Number.isFinite(usage.used_tokens) ||
      usage.used_tokens < 0 ||
      !Number.isFinite(usage.window_tokens) ||
      usage.window_tokens <= 0
    )
      continue;
    latest = usage;
    seq = step.seq;
  }
  return latest;
}

export function contextPercent(usage: ContextUsage): number {
  return Math.round((contextBudgetTokens(usage) / usage.window_tokens) * 100);
}

// The 90%/100% gates use the entire request budget, including reserved output.
export function contextBudgetTokens(usage: ContextUsage): number {
  const reserve = Number.isFinite(usage.reserved_tokens) ? Math.max(0, usage.reserved_tokens) : 0;
  return usage.used_tokens + reserve;
}

export function contextTokens(value: number): string {
  if (value >= 1_000_000) return `${Number((value / 1_000_000).toFixed(2))}M`;
  if (value >= 1000) return `${Number((value / 1000).toFixed(1))}K`;
  return String(value);
}
