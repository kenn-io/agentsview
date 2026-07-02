// Register @testing-library/svelte's per-test setup/cleanup against the
// explicitly imported vitest hooks. The library's own auto-registration only
// fires when afterEach is a global, which it is not here (globals are off), so
// component tests using render() would otherwise leak mounted DOM between
// cases. cleanup() is idempotent, so tests that unmount manually are unharmed.
import "@testing-library/svelte/vitest";
import { initI18n } from "./lib/i18n/index.js";

type StorageName = "localStorage";

function isStorageLike(value: unknown): value is Storage {
  if (value === null || typeof value !== "object") return false;
  const storage = value as Partial<Storage>;
  return (
    typeof storage.clear === "function" &&
    typeof storage.getItem === "function" &&
    typeof storage.key === "function" &&
    typeof storage.removeItem === "function" &&
    typeof storage.setItem === "function"
  );
}

function existingStorage(name: StorageName): Storage | undefined {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, name);
  if (!descriptor || !("value" in descriptor)) return undefined;
  return isStorageLike(descriptor.value) ? descriptor.value : undefined;
}

export function installFallbackStorage(name: StorageName): void {
  if (existingStorage(name)) return;

  const store = new Map<string, string>();
  const storage: Storage = {
    get length() {
      return store.size;
    },
    clear() {
      store.clear();
    },
    getItem(key: string) {
      return store.get(key) ?? null;
    },
    key(index: number) {
      return [...store.keys()][index] ?? null;
    },
    removeItem(key: string) {
      store.delete(key);
    },
    setItem(key: string, value: string) {
      store.set(key, String(value));
    },
  };

  Object.defineProperty(globalThis, name, {
    value: storage,
    configurable: true,
    writable: true,
  });
}

/** jsdom has no ResizeObserver; kit-ui's TopBar/FitStages measure with it.
 * A no-op stub keeps them mountable in tests — measurement-driven collapse
 * simply never fires, so components render their expanded state. */
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

installFallbackStorage("localStorage");
installFallbackResizeObserver();
initI18n();
