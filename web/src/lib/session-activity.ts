import { advanceActivityUnread, type ActivityUnreadState } from "./activity-stream";
import { mergeActivities } from "./activity-merge";
import type { Activity } from "./types";

export type SessionState = ActivityUnreadState & {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean;
  earliestSeq: number;
  lastTs: string;
  error?: string;
};

export function emptySessionState(): SessionState {
  return { items: [], loaded: false, loading: false, loadingMore: false, hasMore: false,
    earliestSeq: 0, unread: 0, unreadSeq: 0, lastTs: "" };
}

export function applySessionActivityBatch(
  current: SessionState, incoming: Activity[], active: boolean, trim: boolean, limit: number,
): SessionState {
  if (!incoming.length) return current;
  let unread: ActivityUnreadState = current;
  for (const step of incoming) unread = advanceActivityUnread(unread, step, active);
  const latest = incoming.reduce((a, b) => a.seq > b.seq ? a : b);
  const lastTs = latest.seq >= current.unreadSeq ? latest.ts : current.lastTs;
  if (!current.loaded && !current.loading && !active) {
    const accounting = incoming.filter((a) => ["usage", "result", "context_usage"].includes(a.kind));
    return { ...current, ...unread, lastTs, items: mergeActivities(current.items, accounting).slice(-4) };
  }
  let items = mergeActivities(current.items, incoming);
  const trimmed = trim && items.length > limit;
  if (trimmed) items = items.slice(-limit);
  return { ...current, ...unread, items, lastTs,
    hasMore: current.hasMore || trimmed, earliestSeq: trimmed ? items[0].seq : current.earliestSeq };
}
