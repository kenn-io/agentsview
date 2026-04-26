import { sessionTiming } from "./sessionTiming.svelte.js";

/** Reactive Date.now() that ticks every second while the loaded
 *  session has `running: true`. A $derived that reads
 *  `liveTick.now` re-runs on each tick, so running-duration
 *  labels can refresh at 1Hz without a per-component setInterval. */
class LiveTickStore {
  now: number = $state(Date.now());

  private timer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    $effect.root(() => {
      $effect(() => {
        const running = sessionTiming.timing?.running ?? false;
        if (running) {
          if (this.timer == null) {
            this.now = Date.now();
            this.timer = setInterval(() => {
              this.now = Date.now();
            }, 1000);
          }
        } else if (this.timer != null) {
          clearInterval(this.timer);
          this.timer = null;
          this.now = Date.now();
        }
      });
    });
  }
}

export const liveTick = new LiveTickStore();
