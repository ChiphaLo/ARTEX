// Bound React commits independently of transport frame rate. The wire history is
// not sampled or dropped. A session/task switch disposes its pending old batch.
export function createActivityBatcher<T>(
  consume: (items: T[]) => void,
  delay: () => number = () => 100,
  schedule: (callback: () => void, ms: number) => ReturnType<typeof setTimeout> = setTimeout,
  cancel: (timer: ReturnType<typeof setTimeout>) => void = clearTimeout,
) {
  let pending: T[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const flush = () => {
    if (timer !== undefined) cancel(timer);
    timer = undefined;
    if (disposed || !pending.length) return;
    const items = pending;
    pending = [];
    consume(items);
  };
  return {
    push(item: T) {
      if (disposed) return;
      pending.push(item);
      if (pending.length >= 1000) flush();
      else if (timer === undefined) timer = schedule(flush, delay());
    },
    flush,
    dispose() {
      disposed = true;
      pending = [];
      if (timer !== undefined) cancel(timer);
      timer = undefined;
    },
  };
}
