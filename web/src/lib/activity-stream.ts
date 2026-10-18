import type { Activity } from "./types";

export type ActivityUnreadState = { unread: number; unreadSeq: number };

// Count persisted, visible activities, not transport frames. A streamed thought
// has many deltas but only one final row; usage markers are never displayed.
// Keep the cursor outside the history cache so cold/trimmed sessions and SSE
// replays obey the same accounting rules as the currently loaded session.
export function advanceActivityUnread(
  state: ActivityUnreadState,
  step: Activity,
  active: boolean,
): ActivityUnreadState {
  const streamedDelta = step.kind === "thinking" && step.metadata?.stream_id && !step.metadata.stream_done;
  const countable = step.kind !== "usage" && !streamedDelta && !step.metadata?.context_excluded;
  return {
    unread: active ? 0 : state.unread + (step.seq > state.unreadSeq && countable ? 1 : 0),
    unreadSeq: Math.max(state.unreadSeq, step.seq),
  };
}

// Delta rows carry text in metadata so SSE and history polling both update immediately.
export function coalesceThinking(steps: Activity[]): Activity[] {
  const streams = new Map<string, Activity>();
  const out: Activity[] = [];
  for (const step of steps) {
    const id = step.metadata?.stream_id;
    if (!id) {
      out.push(step);
      continue;
    }
    const previous = streams.get(id);
    if (step.metadata?.stream_done) {
      if (previous) {
        Object.assign(previous, step, { detail: step.detail });
      } else {
        const row = { ...step };
        streams.set(id, row);
        out.push(row);
      }
    } else {
      const text = (previous?.detail ?? "") + (step.metadata?.stream_delta ?? "");
      if (previous) Object.assign(previous, { ...step, detail: text, summary: text.replace(/\s+/g, " ").slice(-200) });
      else {
        const row = { ...step, detail: text };
        streams.set(id, row);
        out.push(row);
      }
    }
  }
  return out;
}
