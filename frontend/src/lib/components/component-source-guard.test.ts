// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { existsSync, readdirSync, readFileSync } from "node:fs";
// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { relative, resolve } from "node:path";
import { describe, expect, it } from "vite-plus/test";

const componentsRoot = resolve(
  // @ts-ignore -- import.meta.dirname is Node 20.11+, in the supported range.
  import.meta.dirname,
);

const legacyNativeSelectAllowlist = new Set([
  "activity/ActivityInsight.svelte",
  "activity/ActivityPage.svelte",
  "activity/ConcurrencyTimeline.svelte",
]);

function svelteFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry: any) => {
    const path = `${dir}/${entry.name}`;
    if (entry.isDirectory()) return svelteFiles(path);
    if (entry.isFile() && entry.name.endsWith(".svelte")) return [path];
    return [];
  });
}

describe("component source guardrails", () => {
  it("keeps new native select controls out of component source", () => {
    const offenders = svelteFiles(componentsRoot).flatMap((path) => {
      const rel = relative(componentsRoot, path);
      if (legacyNativeSelectAllowlist.has(rel)) return [];

      const source = readFileSync(path, "utf8");
      return /<select\b/.test(source) ? [rel] : [];
    }).sort();

    expect(offenders).toEqual([]);
  });

  it("keeps the legacy select allowlist honest", () => {
    const missing = Array.from(legacyNativeSelectAllowlist).filter((rel) => {
      const path = `${componentsRoot}/${rel}`;
      return !existsSync(path);
    });

    expect(missing).toEqual([]);

    const stale = Array.from(legacyNativeSelectAllowlist).filter((rel) => {
      const source = readFileSync(`${componentsRoot}/${rel}`, "utf8");
      return !/<select\b/.test(source);
    });

    expect(stale).toEqual([]);
  });
});
