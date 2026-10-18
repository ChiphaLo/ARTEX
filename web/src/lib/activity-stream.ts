import type { Activity } from "./types";

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
