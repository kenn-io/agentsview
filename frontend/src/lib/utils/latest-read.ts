export class LatestRead {
  #controller: AbortController | null = null;

  begin(): AbortSignal {
    this.cancel();
    this.#controller = new AbortController();
    return this.#controller.signal;
  }

  isCurrent(signal: AbortSignal): boolean {
    return this.#controller?.signal === signal && !signal.aborted;
  }

  finish(signal: AbortSignal): boolean {
    if (!this.isCurrent(signal)) return false;
    this.#controller = null;
    return true;
  }

  cancel(): void {
    this.#controller?.abort();
    this.#controller = null;
  }
}
