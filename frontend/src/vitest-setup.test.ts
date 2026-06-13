import { afterEach, describe, expect, it } from "vitest";
import { installFallbackStorage } from "./vitest-setup";

describe("vitest setup storage fallback", () => {
  const originalDescriptor = Object.getOwnPropertyDescriptor(
    globalThis,
    "localStorage",
  );

  afterEach(() => {
    if (originalDescriptor) {
      Object.defineProperty(globalThis, "localStorage", originalDescriptor);
    } else {
      delete (globalThis as { localStorage?: Storage }).localStorage;
    }
  });

  it("installs localStorage when the Node global getter returns undefined", () => {
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      get: () => undefined,
    });

    installFallbackStorage("localStorage");

    localStorage.setItem("agentsview-test-key", "value");

    expect(localStorage.getItem("agentsview-test-key")).toBe("value");
    expect(localStorage.length).toBe(1);
    expect(localStorage.key(0)).toBe("agentsview-test-key");
  });

  it("installs localStorage when the Node global value is not Storage-like", () => {
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      value: {},
    });

    installFallbackStorage("localStorage");

    localStorage.setItem("agentsview-test-key", "value");

    expect(localStorage.getItem("agentsview-test-key")).toBe("value");
  });

  it("does not invoke a non-storage localStorage accessor", () => {
    let reads = 0;
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      get: () => {
        reads += 1;
        return {};
      },
    });

    installFallbackStorage("localStorage");

    localStorage.setItem("agentsview-test-key", "value");

    expect(reads).toBe(0);
    expect(localStorage.getItem("agentsview-test-key")).toBe("value");
  });
});
