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
  const countable =
    step.kind !== "usage" &&
    step.kind !== "context_usage" &&
    step.kind !== "model_status" &&
    !streamedDelta &&
    !step.metadata?.context_excluded;
  return {
    unread: active ? 0 : state.unread + (step.seq > state.unreadSeq && countable ? 1 : 0),
    unreadSeq: Math.max(state.unreadSeq, step.seq),
  };
}

// Keep one presentation row per thought. Only the bounded summary tail is
// normalised: scanning the entire accumulated text for EVERY delta was quadratic,
// even when the thought was collapsed. Previous published rows remain immutable.
// Append-only updates reuse completed row objects; prepend/replace/trim/replay
// rebuilds from the authoritative input instead of trusting seq alone.
export class ThinkingProjection {
  private input: Activity[] = [];
  private output: Activity[] = [];
  private streams = new Map<string, { index: number; tail: string }>();

  project(steps: Activity[]): Activity[] {
    if (steps === this.input) return this.output;
    const append = steps.length >= this.input.length && this.input.every((step, i) => step === steps[i]);
    const start = append ? this.input.length : 0;
    if (!append) this.streams.clear();
    const out = append ? [...this.output] : [];
    for (let i = start; i < steps.length; i++) {
      const step = steps[i];
      const id = step.metadata?.stream_id;
      if (!id) {
        out.push(step);
        continue;
      }
      const state = this.streams.get(id);
      const previous = state ? out[state.index] : undefined;
      const chunk = step.metadata?.stream_delta ?? "";
      const done = step.metadata?.stream_done;
      const tail = done ? "" : ((state?.tail ?? "") + chunk).replace(/\s+/g, " ").slice(-200);
      const row = done
        ? { ...step } // A final row without inline detail must lazy-load the full body.
        : { ...step, detail: (previous?.detail ?? "") + chunk, summary: previous ? tail : step.summary };
      const index = state?.index ?? out.length;
      out[index] = row;
      this.streams.set(id, { index, tail });
    }
    this.input = steps;
    this.output = out;
    return out;
  }
}

export function coalesceThinking(steps: Activity[]): Activity[] {
  return new ThinkingProjection().project(steps);
}
