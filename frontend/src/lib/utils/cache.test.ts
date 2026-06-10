import { describe, it, expect } from "vite-plus/test";
import { LRUCache } from "./cache.js";

describe("LRUCache", () => {
  it("stores and retrieves values", () => {
    const c = new LRUCache<string, number>(3);
    c.set("a", 1);
    expect(c.get("a")).toBe(1);
  });

  it("returns undefined for missing keys", () => {
    const c = new LRUCache<string, number>(3);
    expect(c.get("missing")).toBeUndefined();
  });

  it("evicts the least recently used entry on overflow", () => {
    const c = new LRUCache<string, number>(3);
    c.set("a", 1);
    c.set("b", 2);
    c.set("c", 3);
    c.set("d", 4); // evicts "a"
    expect(c.get("a")).toBeUndefined();
    expect(c.get("b")).toBe(2);
    expect(c.get("d")).toBe(4);
  });

  it("promotes accessed entries so they survive eviction", () => {
    const c = new LRUCache<string, number>(3);
    c.set("a", 1);
    c.set("b", 2);
    c.set("c", 3);
    c.get("a"); // promote "a" — now "b" is least recent
    c.set("d", 4); // evicts "b"
    expect(c.get("a")).toBe(1);
    expect(c.get("b")).toBeUndefined();
  });

  it("updates existing keys without growing size", () => {
    const c = new LRUCache<string, number>(2);
    c.set("a", 1);
    c.set("b", 2);
    c.set("a", 10); // update, not insert
    expect(c.size).toBe(2);
    expect(c.get("a")).toBe(10);
  });

  it("reports size correctly", () => {
    const c = new LRUCache<string, number>(5);
    expect(c.size).toBe(0);
    c.set("a", 1);
    c.set("b", 2);
    expect(c.size).toBe(2);
  });

  it("set refreshes recency of existing key", () => {
    const c = new LRUCache<string, number>(3);
    c.set("a", 1);
    c.set("b", 2);
    c.set("c", 3);
    c.set("a", 10); // refresh "a" — now "b" is LRU
    c.set("d", 4); // evicts "b"
    expect(c.get("b")).toBeUndefined();
    expect(c.get("a")).toBe(10);
    expect(c.get("c")).toBe(3);
    expect(c.get("d")).toBe(4);
  });

  it("evicts in insertion order without any access", () => {
    const c = new LRUCache<string, number>(2);
    c.set("a", 1);
    c.set("b", 2);
    c.set("c", 3); // evicts "a"
    c.set("d", 4); // evicts "b"
    expect(c.get("a")).toBeUndefined();
    expect(c.get("b")).toBeUndefined();
    expect(c.get("c")).toBe(3);
    expect(c.get("d")).toBe(4);
    expect(c.size).toBe(2);
  });

  it("mixed get/set maintains correct eviction order", () => {
    const c = new LRUCache<string, number>(3);
    c.set("a", 1);
    c.set("b", 2);
    c.set("c", 3);
    // Access order: a(set), b(set), c(set) → LRU is "a"
    c.get("a"); // promote "a" → LRU is "b"
    c.set("b", 20); // refresh "b" → LRU is "c"
    c.set("d", 4); // evicts "c"
    expect(c.get("c")).toBeUndefined();
    expect(c.get("a")).toBe(1);
    expect(c.get("b")).toBe(20);
    expect(c.get("d")).toBe(4);
  });

  it("capacity of one evicts on every new key", () => {
    const c = new LRUCache<string, number>(1);
    c.set("a", 1);
    expect(c.get("a")).toBe(1);
    c.set("b", 2);
    expect(c.get("a")).toBeUndefined();
    expect(c.get("b")).toBe(2);
    expect(c.size).toBe(1);
  });

  it("stores and retrieves undefined values", () => {
    const c = new LRUCache<string, undefined>(3);
    c.set("a", undefined);
    c.set("b", undefined);
    c.set("c", undefined);
    expect(c.get("a")).toBeUndefined();
    expect(c.size).toBe(3);
  });

  it("promotes entries with undefined values on get", () => {
    const c = new LRUCache<string, number | undefined>(2);
    c.set("a", undefined);
    c.set("b", 1);
    c.get("a"); // promote "a" — now "b" is LRU
    c.set("c", 2); // evicts "b"
    expect(c.get("b")).toBeUndefined();
    expect(c.size).toBe(2);
    expect(c.get("a")).toBeUndefined();
    // "a" is still present — verify by checking size after access
    expect(c.size).toBe(2);
  });

  it("throws on zero capacity", () => {
    expect(() => new LRUCache<string, number>(0)).toThrow(
      /capacity must be a positive integer/,
    );
  });

  it("throws on negative capacity", () => {
    expect(() => new LRUCache<string, number>(-1)).toThrow(
      /capacity must be a positive integer/,
    );
  });

  it("throws on non-integer capacity", () => {
    expect(() => new LRUCache<string, number>(2.5)).toThrow(
      /capacity must be a positive integer/,
    );
  });
});
