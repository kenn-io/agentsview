// Register @testing-library/svelte's per-test setup/cleanup against the
// explicitly imported vitest hooks. The library's own auto-registration only
// fires when afterEach is a global, which it is not here (globals are off), so
// component tests using render() would otherwise leak mounted DOM between
// cases. cleanup() is idempotent, so tests that unmount manually are unharmed.
import "@testing-library/svelte/vitest";
import { initI18n } from "./lib/i18n/index.js";

export function installFallbackResizeObserver(): void {
  if (typeof globalThis.ResizeObserver !== "undefined") return;

  class FallbackResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }

  Object.defineProperty(globalThis, "ResizeObserver", {
    value: FallbackResizeObserver,
    configurable: true,
    writable: true,
  });
}

/** jsdom has no pointer capture; kit-ui's SplitResizeHandle captures the
 * pointer on drag start. Inert stubs keep drags dispatchable in tests —
 * events are delivered by plain dispatch on the handle, so capture-based
 * retargeting is not needed. */
export function installFallbackPointerCapture(): void {
  const proto = globalThis.Element?.prototype;
  if (!proto || typeof proto.setPointerCapture === "function") return;

  Object.assign(proto, {
    setPointerCapture(): void {},
    releasePointerCapture(): void {},
    hasPointerCapture(): boolean {
      return false;
    },
  });
}

/** Svelte motion reads prefers-reduced-motion during module initialization.
 * jsdom does not implement matchMedia, so provide the inert browser shape. */
export function installFallbackMatchMedia(): void {
  if (typeof window.matchMedia === "function") return;

  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string): MediaQueryList => ({
      matches: false,
      media: query,
      onchange: null,
      addListener() {},
      removeListener() {},
      addEventListener() {},
      removeEventListener() {},
      dispatchEvent: () => false,
    }),
  });
}

// Node's own localStorage getter shadows jsdom's and returns undefined without
// --localstorage-file. Use jsdom's Storage so StorageEvent.storageArea matches.
const { jsdom } = globalThis as typeof globalThis & { jsdom: { window: Window } };
Object.defineProperty(globalThis, "localStorage", {
  value: jsdom.window.localStorage,
  configurable: true,
  writable: true,
});
installFallbackResizeObserver();
installFallbackPointerCapture();
installFallbackMatchMedia();
initI18n();
