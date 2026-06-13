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

installFallbackStorage("localStorage");
