import type { Activity } from "./types";

// History and live responses can overlap or arrive out of order. A persisted
// activity's seq is its identity; replay must not duplicate rows or token totals.
export function mergeActivities(current: Activity[], incoming: Activity[]): Activity[] {
  if (!incoming.length) return current;
  // The normal live tail is already strictly ordered and does not overlap.
  // Keep the map/sort fallback for history, reconnects and row replacements.
  if ((!current.length || current[current.length - 1].seq < incoming[0].seq) &&
      incoming.every((item, i) => i === 0 || incoming[i - 1].seq < item.seq)) {
    return current.concat(incoming);
  }
  const bySeq = new Map<number, Activity>();
  for (const activity of current) bySeq.set(activity.seq, activity);
  for (const activity of incoming) bySeq.set(activity.seq, activity);
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}
