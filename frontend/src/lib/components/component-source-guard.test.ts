// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { readdirSync, readFileSync } from "node:fs";
// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { relative, resolve } from "node:path";
import { describe, expect, it } from "vite-plus/test";

const componentsRoot = resolve(
  // @ts-ignore -- import.meta.dirname is Node 20.11+, in the supported range.
  import.meta.dirname,
);

function svelteFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry: any) => {
    const path = `${dir}/${entry.name}`;
    if (entry.isDirectory()) return svelteFiles(path);
    if (entry.isFile() && entry.name.endsWith(".svelte")) return [path];
    return [];
  });
}

function styleBlocks(source: string): string[] {
  return Array.from(source.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style>/g)).map(
    (match) => match[1] ?? "",
  );
}

function oneOffControlStyleOffenders(source: string): string[] {
  return styleBlocks(source).flatMap((style) => {
    const offenders: string[] = [];
    if (/\.[A-Za-z0-9_-]+-select(?=[\s:{,.#>+~[])/.test(style)) {
      offenders.push("component-specific *-select selector");
    }
    if (/\.[A-Za-z0-9_-]+-select-chevron(?=[\s:{,.#>+~[])/.test(style)) {
      offenders.push("manual select chevron selector");
    }
    if (/(?:^|\s)-?(?:webkit-)?appearance\s*:\s*none\b/.test(style)) {
      offenders.push("manual native control appearance reset");
    }
    return offenders;
  });
}

describe("component source guardrails", () => {
  it("keeps new native select controls out of component source", () => {
    const offenders = svelteFiles(componentsRoot)
      .flatMap((path) => {
        const rel = relative(componentsRoot, path);
        const source = readFileSync(path, "utf8");
        return /<select\b/.test(source) ? [rel] : [];
      })
      .sort((a, b) => a.localeCompare(b));

    expect(offenders).toEqual([]);
  });

  it("keeps one-off select chrome out of component styles", () => {
    const offenders = svelteFiles(componentsRoot)
      .flatMap((path) => {
        const rel = relative(componentsRoot, path);
        const source = readFileSync(path, "utf8");
        return oneOffControlStyleOffenders(source).map((reason) => `${rel}: ${reason}`);
      })
      .sort((a, b) => a.localeCompare(b));

    expect(offenders).toEqual([]);
  });
});
