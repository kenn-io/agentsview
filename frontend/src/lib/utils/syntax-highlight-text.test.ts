// @vitest-environment jsdom
import { describe, expect, it } from "vite-plus/test";
import { highlightToHtml } from "./syntax-highlight.js";

describe("syntax highlighting source fidelity", () => {
  it.each([
    ["empty source", ""],
    ["no final newline", "const value = 42;"],
    ["final newline", "const value = 42;\n"],
    ["leading newline", "\nconst value = 42;"],
    ["multiple blank lines", "\n\nconst value = 42;\n\n"],
    ["whitespace only", " \t\n\n\t "],
    ["indented multiline code", "if (true) {\n\tconst value = 42;  \n}\n"],
    ["Windows line endings", "const first = 1;\r\nconst second = 2;\r\n"],
    ["carriage return line endings", "const first = 1;\rconst second = 2;\r"],
    ["mixed line endings", "\r\nconst first = 1;\rconst second = 2;\n\n"],
    ["literal HTML", 'const text = "<br><script>hello</script>&amp;";\n'],
    ["Unicode source", 'const text = "\u4e2d\u6587\u{1F680}";\n'],
  ])("preserves %s in the DOM text", async (_label, code) => {
    const html = await highlightToHtml(code, "typescript");
    expect(html).not.toBeNull();
    const element = document.createElement("code");
    element.innerHTML = html!;
    expect(element.textContent).toBe(code);
    expect(element.querySelector("br, script")).toBeNull();
  });

  it("retains separators so search cannot join identifiers on adjacent lines", async () => {
    const html = await highlightToHtml("first\nsecond\n", "typescript");
    expect(html).not.toBeNull();
    const element = document.createElement("code");
    element.innerHTML = html!;
    expect(element.textContent).not.toContain("firstsecond");
    expect(element.textContent?.indexOf("second")).toBe(6);
  });
});
