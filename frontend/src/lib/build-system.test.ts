import { describe, expect, it } from "vite-plus/test";
// @ts-ignore -- @types/node is not in devDependencies; harmless at runtime.
import { readFileSync } from "node:fs";

describe("build system package manager usage", () => {
  it("uses npm ci instead of npm install in Makefile build targets", () => {
    const makefile = readFileSync("../Makefile", "utf8");

    expect(makefile).not.toContain("npm install");
    expect(makefile).toContain("npm ci && npm run build");
    expect(makefile).toContain("npm ci && npm run tauri:build");
  });

  it("uses npm ci in desktop helper build scripts", () => {
    const scripts = ["../scripts/desktop-dev.ps1", "../desktop/scripts/prepare-sidecar.sh"];

    for (const script of scripts) {
      const source = readFileSync(script, "utf8");
      expect(source, script).not.toContain("npm install");
      expect(source, script).toContain("npm ci");
    }
  });
});
