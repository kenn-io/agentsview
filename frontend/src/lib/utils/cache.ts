/**
 * Map-based LRU cache. Exploits the insertion-order guarantee
 * of ES2015 Map: the least-recently-used entry is always first.
 */
export class LRUCache<K, V> {
  private map = new Map<K, V>();

  constructor(private capacity: number) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new Error(`LRUCache capacity must be a positive integer, got ${capacity}`);
    }
  }

  get(key: K): V | undefined {
    if (!this.map.has(key)) return undefined;
    const value = this.map.get(key)!;
    // Delete and re-insert to mark as most recently used
    this.map.delete(key);
    this.map.set(key, value);
    return value;
  }

  set(key: K, value: V): void {
    if (this.map.has(key)) {
      this.map.delete(key);
    } else if (this.map.size >= this.capacity) {
      this.map.delete(this.map.keys().next().value!);
    }
    this.map.set(key, value);
  }

  clear(): void {
    this.map.clear();
  }

  get size(): number {
    return this.map.size;
  }
}
