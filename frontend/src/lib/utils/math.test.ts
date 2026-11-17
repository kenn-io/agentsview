import { describe, expect, it } from "vite-plus/test";
import { renderMarkdown } from "./markdown.js";
import { parseContent } from "./content-parser.js";
import { domText } from "../search/dom-text.js";
import { mapOccurrences } from "../search/search-block.svelte.js";

const fence = "`".repeat(3);
const code = "`";

function render(source: string, preformatted = false): HTMLElement {
  const root = document.createElement("div");
  root.innerHTML = renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: preformatted });
  return root;
}

describe.each([false, true])("math with preformatted XML %s", (preformatted) => {
  it.each(["$e^{i\\pi}+1=0$", "\\(e^{i\\pi}+1=0\\)", "\\begin{math}e^{i\\pi}+1=0\\end{math}"])(
    "renders inline %s",
    (source) => {
      const root = render("Before " + source + ", after.", preformatted);
      expect(root.querySelectorAll(".katex")).toHaveLength(1);
      expect(root.querySelector(".katex-display")).toBeNull();
      expect(root.querySelector("math")).not.toBeNull();
      expect(root.textContent).toContain("Before ");
      expect(root.textContent).toContain(", after.");
    },
  );

  it.each([
    "$$\\frac{1}{3}$$",
    "$$\n\\frac{1}{3}\n$$",
    "\\[\\frac{1}{3}\\]",
    "\\[\n\\frac{1}{3}\n\\]",
    "\\begin{displaymath}\\frac{1}{3}\\end{displaymath}",
    "\\begin{equation}\\frac{1}{3}\\end{equation}",
    "\\begin{equation*}\\frac{1}{3}\\end{equation*}",
    "\\begin{align}a&=b\\\\c&=d\\end{align}",
    "\\begin{align*}a&=b\\\\c&=d\\end{align*}",
    "\\begin{alignat}{2}a&=b&c&=d\\end{alignat}",
    "\\begin{alignat*}{2}a&=b&c&=d\\end{alignat*}",
    "\\begin{gather}a=b\\\\c=d\\end{gather}",
    "\\begin{gather*}a=b\\\\c=d\\end{gather*}",
    "\\begin{aligned}a&=b\\\\c&=d\\end{aligned}",
    "\\begin{pmatrix}a&b\\\\c&d\\end{pmatrix}",
  ])("renders display %s", (source) => {
    const root = render("Before\n" + source + "\nAfter", preformatted);
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(root.textContent).toContain("Before");
    expect(root.textContent).toContain("After");
  });

  it.each([
    ["bare", "", ""],
    ["dollars", "$$", "$$"],
    ["brackets", "\\[", "\\]"],
    ["fenced", fence + "math\n", "\n" + fence],
  ])("renders the complete nested matrix in %s math", (_label, left, right) => {
    const source = "\\begin{pmatrix}\\begin{pmatrix}a&b\\\\c&d\\end{pmatrix}&z\\end{pmatrix}";
    const root = render("Before\n\n" + left + source + right + "\n\nAfter", preformatted);
    expect(root.querySelectorAll(".katex")).toHaveLength(1);
    expect(Array.from(root.querySelectorAll("math mi"), (cell) => cell.textContent)).toEqual([
      "a",
      "b",
      "c",
      "d",
      "z",
    ]);
    expect(root.querySelector("mtable mtable")).not.toBeNull();
    expect(root.querySelector("math")?.textContent).toContain("z");
    root.querySelector(".katex-display")?.remove();
    expect(root.textContent?.trim()).toBe("Before\nAfter");
  });

  it.each([
    "\\begin{pmatrix}% \\begin{pmatrix} ignored\n\\begin{pmatrix}a\\end{pmatrix}&z\\end{pmatrix}",
    "\\begin{pmatrix}\\begin{pmatrix}a\\end{pmatrix}% \\end{pmatrix} ignored\n&z\\end{pmatrix}",
    "\\begin{pmatrix}a\\\\begin{pmatrix}\\\\z\\end{pmatrix}",
    "\\begin{pmatrix}a\\\\end{pmatrix}\\\\z\\end{pmatrix}",
  ])("ignores commented and escaped environment commands: %s", (source) => {
    const root = render(source, preformatted);
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(root.querySelector("math")?.textContent).toContain("z");
    root.querySelector(".katex-display")?.remove();
    expect(root.textContent?.trim()).toBe("");
  });

  it("preserves the original conversation's aligned Maxwell equations", () => {
    const source =
      "$$\n\\begin{aligned}\n" +
      "\\nabla \\cdot \\mathbf{E} &= \\frac{\\rho}{\\varepsilon_0},\\\\\n" +
      "\\nabla \\times \\mathbf{B} &= \\mu_0\\mathbf{J}+\\mu_0\\varepsilon_0\\frac{\\partial \\mathbf{E}}{\\partial t}.\n" +
      "\\end{aligned}\n$$";
    const root = render(source, preformatted);
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(root.querySelectorAll(".mfrac").length).toBeGreaterThan(0);
  });

  it.each([
    ["\\[", "\\]"],
    ["$$", "$$"],
    ["\\begin{equation}", "\\end{equation}"],
  ])("protects standalone equals lines inside %s math from heading parsing", (left, right) => {
    const source = `The objective satisfies\n${left}\nQ(x)\n=\n\\frac{p(x)}{q(x)}\n${right}\nAfter.`;
    const root = render(source, preformatted);
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(root.querySelector(".mfrac")).not.toBeNull();
    expect(root.querySelector("h1, h2")).toBeNull();
    expect(root.querySelectorAll("p")[0]?.textContent).toBe("The objective satisfies");
    expect(root.querySelectorAll("p")[1]?.textContent).toBe("After.");
  });

  it("keeps a multiline equation inside its numbered list item", () => {
    const root = render(
      "1. **Prepare the state.** Use\n" +
        "   \\[\n   \\rho_x\n   =\n   \\frac{\\sqrt{\\eta}\\,\\Pi_x\\,\\sqrt{\\eta}}\n" +
        "        {p(x)}.\n   \\]\n   Check the result.\n\n2. Continue.",
      preformatted,
    );
    expect(root.querySelectorAll("ol > li")).toHaveLength(2);
    expect(root.querySelectorAll("li:first-child .katex-display")).toHaveLength(1);
    expect(root.querySelector("li:first-child .mfrac")).not.toBeNull();
    expect(root.querySelector("h1, h2, pre")).toBeNull();
    expect(root.querySelector("li:first-child")?.textContent).toContain("Check the result.");
    expect(root.querySelector("li:last-child")?.textContent?.trim()).toBe("Continue.");
  });

  it("preserves ordinary underlined headings around display math", () => {
    const root = render("Before\n======\n\\[\nx\n=\ny\n\\]\nAfter\n-----\n", preformatted);
    expect(root.querySelector("h1")?.textContent).toBe("Before");
    expect(root.querySelector("h2")?.textContent).toBe("After");
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
  });

  it("keeps delimiters inside groups and comments within the math token", () => {
    const root = render("$$\\text{price: \\$5} % $$ ignored\n+ x$$", preformatted);
    expect(root.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(root.textContent).toContain("price:");
    expect(domText(root)).not.toContain("ignored");
  });

  it("handles punctuation, adjacent expressions and CJK boundaries", () => {
    const root = render("($x$),中文\\(y\\)，$z$.$a$$b$", preformatted);
    expect(root.querySelectorAll(".katex")).toHaveLength(5);
  });

  it("renders math in lists, quotes and tables", () => {
    const root = render("- $x$\n\n> \\(y\\)\n\n| value |\n| --- |\n| $z$ |", preformatted);
    expect(root.querySelectorAll(".katex")).toHaveLength(3);
    expect(root.querySelector("table .katex")).not.toBeNull();
  });

  it.each(["$5 and $10", "\\$x\\$", "$unclosed", "\\[unclosed", "$ x $"])(
    "keeps non-math input readable: %s",
    (source) => {
      const root = render(source, preformatted);
      expect(root.querySelector(".katex")).toBeNull();
      expect(root.textContent).not.toBe("");
    },
  );

  it("preserves inline and ordinary fenced code", () => {
    const root = render(
      code + "$x$ \\(y\\)" + code + "\n\n" + fence + "python\n$z$ \\[w\\]\n" + fence,
      preformatted,
    );
    expect(root.querySelector(".katex")).toBeNull();
    expect(root.querySelectorAll("code")).toHaveLength(2);
  });

  it("falls back to the original source for unsupported commands", () => {
    const root = render("Before $\\notARealCommand{x}$ after $y$.", preformatted);
    expect(root.textContent).toContain("$\\notARealCommand{x}$");
    expect(root.querySelectorAll(".katex")).toHaveLength(1);
  });

  it("does not trust TeX HTML commands or interpolate HTML from failed math", () => {
    const root = render(
      "$\\href{javascript:alert(1)}{click}$ " +
        "$\\htmlClass{injected}{x}$ " +
        "$\\bad{<img src=x onerror=alert(1)>}$",
      preformatted,
    );
    expect(root.querySelector("a, img, .injected, [onerror]")).toBeNull();
    expect(root.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("bounds recursive macro expansion without poisoning the next formula", () => {
    const root = render(
      "$\\def\\loop{\\loop}\\loop$ then $x$ " + "$\\gdef\\foo{z}\\foo$ then $\\foo$",
      preformatted,
    );
    expect(root.textContent).toContain("\\loop");
    expect(root.textContent).toContain("$\\foo$");
    expect(root.querySelectorAll(".katex")).toHaveLength(2);
  });

  it.each(["math", "latex", "tex", "MATH"])(
    "renders %s fences through message segmentation",
    (language) => {
      const source = fence + language + "\n\\frac{a}{b}\n" + fence;
      const segments = parseContent(source, false);
      expect(segments).toHaveLength(1);
      expect(segments[0]!.type).toBe("text");
      const root = render(segments[0]!.content, preformatted);
      expect(root.querySelector(".katex-display")).not.toBeNull();
      expect(root.querySelector("pre")).toBeNull();
    },
  );

  it("accepts delimiters within a math fence and leaves full documents as code", () => {
    expect(
      render(fence + "math\n\\[x^2\\]\n" + fence, preformatted).querySelector(".katex-display"),
    ).not.toBeNull();
    const root = render(fence + "latex\n\\documentclass{article}\n" + fence, preformatted);
    expect(root.querySelector("pre code")?.textContent).toContain("\\documentclass");
    expect(parseContent(fence + "latex\n\\documentclass{article}\n" + fence, false)[0]!.type).toBe(
      "code",
    );
  });
});

it("renders GitHub's inline math syntax", () => {
  const root = render("$" + code + "x^2" + code + "$");
  expect(root.querySelector(".katex")).not.toBeNull();
  expect(domText(root)).not.toContain(code);
});

it("searches the visible math once and maps subsequent highlights correctly", () => {
  const root = render("Before $x$ after");
  expect(domText(root).trimEnd()).toBe("Before x after");
  const [boundaries] = mapOccurrences(root, [{ start: 9, end: 14 }]);
  const range = document.createRange();
  range.setStart(boundaries!.startContainer, boundaries!.startOffset);
  range.setEnd(boundaries!.endContainer, boundaries!.endOffset);
  expect(range.toString()).toBe("after");
});

it("does not pair a currency dollar with a delimiter inside inline code", () => {
  const root = render("Prices: $5 and $10. Literal: " + code + "$x$" + code);
  expect(root.querySelector(".katex")).toBeNull();
  expect(root.querySelector("code")?.textContent).toBe("$x$");
});
