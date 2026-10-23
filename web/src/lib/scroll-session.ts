// A shared viewport is reused across agent sessions. Old pagination requests
// must never restore their scroll position into a newly selected session.
export class ScrollSession {
  private key = "";
  private epoch = 0;
  private intentUntil = 0;
  select(key: string) {
    if (key !== this.key) {
      this.key = key;
      this.epoch++;
      this.intentUntil = 0;
    }
  }
  ticket() {
    return { key: this.key, epoch: this.epoch };
  }
  isCurrent(ticket: { key: string; epoch: number }) {
    return ticket.key === this.key && ticket.epoch === this.epoch;
  }
  input(now = Date.now()) {
    this.intentUntil = now + 1000;
  }
  tracksScroll(now = Date.now()) {
    return now < this.intentUntil;
  }
}
export function listenForScrollInput(element: HTMLElement, session: ScrollSession) {
  const input = () => session.input();
  const keys = (e: KeyboardEvent) => {
    if (["ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End", " "].includes(e.key)) input();
  };
  const drag = (e: PointerEvent) => {
    if (e.buttons) input();
  };
  element.addEventListener("wheel", input, { passive: true });
  element.addEventListener("touchmove", input, { passive: true });
  element.addEventListener("pointerdown", input, { passive: true });
  element.addEventListener("pointermove", drag, { passive: true });
  element.addEventListener("keydown", keys);
  return () => {
    element.removeEventListener("wheel", input);
    element.removeEventListener("touchmove", input);
    element.removeEventListener("pointerdown", input);
    element.removeEventListener("pointermove", drag);
    element.removeEventListener("keydown", keys);
  };
}
