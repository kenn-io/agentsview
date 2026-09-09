import { describe, it, expect } from "vite-plus/test";
import issueReproductionFixture from "./__fixtures__/unknown-xml-1672.txt?raw";
import { renderMarkdown } from "./markdown.js";

/**
 * Parse HTML string into a DOM container for semantic assertions.
 * Avoids brittle exact-string comparisons that break on harmless
 * formatting changes in the renderer or sanitizer.
 */
function parseHTML(html: string): HTMLElement {
  const div = document.createElement("div");
  div.innerHTML = html;
  return div;
}

/**
 * Parse HTML as a full document so special elements like <body>
 * are handled correctly (fragment parsing ignores them).
 */
function parseFullDocument(html: string): Document {
  return new DOMParser().parseFromString(html, "text/html");
}

/**
 * Normalize an href value for security checking. Iteratively
 * decodes HTML entities and percent-encoding (up to 5 passes)
 * until stable, strips control characters, and lowercases — so
 * mixed/nested obfuscation like `%26#106%3Bavascript:` or
 * `&#106;avascript:` is fully resolved and detected.
 */
function tolerantDecodeURI(s: string): string {
  return s.replace(/%[0-9A-Fa-f]{2}/g, (m) => {
    try {
      return decodeURIComponent(m);
    } catch {
      return m;
    }
  });
}

function normalizeHref(raw: string): string {
  const txt = document.createElement("textarea");
  let prev = raw;
  const maxPasses = 5;
  for (let i = 0; i < maxPasses; i++) {
    // HTML entity decode
    txt.innerHTML = prev;
    let cur = txt.value;
    // Strip control characters
    // oxlint-disable-next-line no-control-regex -- intentional: strips control chars used to obfuscate dangerous href schemes
    cur = cur.replace(/[\x00-\x1f\x7f]/g, "");
    // Tolerant percent decode (valid %xx chunks only)
    cur = tolerantDecodeURI(cur);
    if (cur === prev) break;
    prev = cur;
  }
  return prev.toLowerCase();
}

/**
 * Assert that no anchor in the rendered HTML has an href matching
 * the given dangerous scheme pattern. Always runs a raw-HTML scan
 * (including unquoted attribute values) in addition to parsed
 * anchor checks, so dangerous hrefs are caught even when the DOM
 * parser strips or transforms them.
 */
function assertNoAnchorScheme(html: string, scheme: RegExp): void {
  const dom = parseHTML(html);
  const anchors = dom.querySelectorAll("a");
  for (const a of anchors) {
    if (!a.hasAttribute("href")) continue;
    const href = a.getAttribute("href") ?? "";
    const norm = normalizeHref(href);
    expect(norm).not.toMatch(scheme);
  }
  // Always scan raw HTML for href values that the DOM parser
  // may not surface as <a> elements (e.g. stripped tags).
  // Matches quoted and unquoted href attribute values.
  const hrefPattern = /\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))/gi;
  let match: RegExpExecArray | null;
  while ((match = hrefPattern.exec(html)) !== null) {
    const value = match[1] ?? match[2] ?? match[3] ?? "";
    const norm = normalizeHref(value);
    expect(norm).not.toMatch(scheme);
  }
}

describe("renderMarkdown", () => {
  describe("inline formatting", () => {
    it("renders bold text", () => {
      const dom = parseHTML(renderMarkdown("**bold**"));
      const strong = dom.querySelector("p > strong");
      expect(strong).not.toBeNull();
      expect(strong!.textContent).toBe("bold");
    });

    it("renders italic text", () => {
      const dom = parseHTML(renderMarkdown("*italic*"));
      const em = dom.querySelector("p > em");
      expect(em).not.toBeNull();
      expect(em!.textContent).toBe("italic");
    });

    it("renders inline code", () => {
      const dom = parseHTML(renderMarkdown("`code`"));
      const code = dom.querySelector("p > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("code");
    });

    it("renders links", () => {
      const dom = parseHTML(renderMarkdown("[text](https://example.com)"));
      const a = dom.querySelector("p > a");
      expect(a).not.toBeNull();
      expect(a!.textContent).toBe("text");
      expect(a!.getAttribute("href")).toBe("https://example.com");
    });
  });

  describe("block elements", () => {
    it("renders headings", () => {
      const dom = parseHTML(renderMarkdown("## Heading 2"));
      const h2 = dom.querySelector("h2");
      expect(h2).not.toBeNull();
      expect(h2!.textContent).toBe("Heading 2");
    });

    it("renders unordered lists", () => {
      const dom = parseHTML(renderMarkdown("- item one\n- item two"));
      const items = dom.querySelectorAll("ul > li");
      expect(items).toHaveLength(2);
      expect(items[0]!.textContent).toBe("item one");
      expect(items[1]!.textContent).toBe("item two");
    });

    it("renders ordered lists", () => {
      const dom = parseHTML(renderMarkdown("1. first\n2. second"));
      const items = dom.querySelectorAll("ol > li");
      expect(items).toHaveLength(2);
      expect(items[0]!.textContent).toBe("first");
      expect(items[1]!.textContent).toBe("second");
    });

    it("renders blockquotes", () => {
      const dom = parseHTML(renderMarkdown("> quoted text"));
      const bq = dom.querySelector("blockquote");
      expect(bq).not.toBeNull();
      expect(bq!.textContent!.trim()).toBe("quoted text");
    });

    it("preserves prose around separated blockquotes", () => {
      const dom = parseHTML(
        renderMarkdown("blabla1\n\n> blabla2\n\nblabla3\n\n> blabla4\n\nblabla5"),
      );
      expect(dom.textContent).toContain("blabla1");
      expect(dom.textContent).toContain("blabla2");
      expect(dom.textContent).toContain("blabla3");
      expect(dom.textContent).toContain("blabla4");
      expect(dom.textContent).toContain("blabla5");
      expect(dom.querySelectorAll("blockquote")).toHaveLength(2);
    });

    it("renders tables", () => {
      const md = "| A | B |\n| --- | --- |\n| 1 | 2 |";
      const dom = parseHTML(renderMarkdown(md));
      const ths = dom.querySelectorAll("thead th");
      expect(ths).toHaveLength(2);
      expect(ths[0]!.textContent).toBe("A");
      expect(ths[1]!.textContent).toBe("B");
      const tds = dom.querySelectorAll("tbody td");
      expect(tds).toHaveLength(2);
      expect(tds[0]!.textContent).toBe("1");
      expect(tds[1]!.textContent).toBe("2");
    });

    it("renders horizontal rules", () => {
      const dom = parseHTML(renderMarkdown("---"));
      expect(dom.querySelector("hr")).not.toBeNull();
    });

    it("converts single newlines to <br>", () => {
      const dom = parseHTML(renderMarkdown("line one\nline two"));
      const p = dom.querySelector("p");
      expect(p).not.toBeNull();
      expect(p!.querySelector("br")).not.toBeNull();
      expect(p!.textContent).toBe("line oneline two");
    });
  });

  describe("security and sanitization", () => {
    it("strips script tags (XSS)", () => {
      expect(renderMarkdown('<script>alert("xss")</script>')).toBe("");
    });

    it("strips event handlers (XSS)", () => {
      const dom = parseHTML(renderMarkdown('<img src=x onerror="alert(1)">'));
      const img = dom.querySelector("img");
      expect(img).not.toBeNull();
      expect(img!.hasAttribute("onerror")).toBe(false);
    });

    it("strips javascript: URLs (XSS)", () => {
      const dom = parseHTML(renderMarkdown("[click](javascript:alert(1))"));
      const a = dom.querySelector("a");
      expect(a).not.toBeNull();
      expect(a!.textContent).toBe("click");
      expect(a!.hasAttribute("href")).toBe(false);
    });

    const xssPayloads: Array<{
      name: string;
      input: string;
      assert: (html: string) => void;
    }> = [
      {
        name: "mixed-case javascript: URL",
        input: "[click](jAvAsCrIpT:alert(1))",
        assert(html) {
          const dom = parseHTML(html);
          const a = dom.querySelector("a");
          expect(a).not.toBeNull();
          expect(a!.hasAttribute("href")).toBe(false);
        },
      },
      {
        name: "tab-padded javascript: URL",
        input: "[click](java\tscript:alert(1))",
        assert(html) {
          assertNoAnchorScheme(html, /^javascript:/);
        },
      },
      {
        name: "newline-padded javascript: URL",
        input: "[click](java\nscript:alert(1))",
        assert(html) {
          assertNoAnchorScheme(html, /^javascript:/);
        },
      },
      {
        name: "URL-encoded javascript: scheme",
        input: "[click](&#106;avascript:alert(1))",
        assert(html) {
          assertNoAnchorScheme(html, /^javascript:/);
        },
      },
      {
        name: "data: text/html payload",
        input: "[click](data:text/html,<script>alert(1)</script>)",
        assert(html) {
          assertNoAnchorScheme(html, /^data:/);
        },
      },
      {
        name: "data: base64 payload",
        input: "[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)",
        assert(html) {
          assertNoAnchorScheme(html, /^data:/);
        },
      },
      {
        name: "vbscript: URL",
        input: "[click](vbscript:MsgBox(1))",
        assert(html) {
          assertNoAnchorScheme(html, /^vbscript:/);
        },
      },
      {
        name: "onload event handler on body tag",
        input: '<body onload="alert(1)">',
        assert(html) {
          const doc = parseFullDocument(html);
          for (const el of doc.querySelectorAll("*")) {
            expect(el.hasAttribute("onload")).toBe(false);
          }
        },
      },
      {
        name: "onfocus event handler with autofocus",
        input: '<input onfocus="alert(1)" autofocus>',
        assert(html) {
          const dom = parseHTML(html);
          for (const el of dom.querySelectorAll("*")) {
            expect(el.hasAttribute("onfocus")).toBe(false);
          }
        },
      },
      {
        name: "SVG with onload",
        input: '<svg onload="alert(1)">',
        assert(html) {
          const dom = parseHTML(html);
          for (const el of dom.querySelectorAll("*")) {
            expect(el.hasAttribute("onload")).toBe(false);
          }
        },
      },
    ];

    it.each(xssPayloads)("sanitizes $name", ({ input, assert: assertFn }) => {
      assertFn(renderMarkdown(input));
    });
  });

  describe("custom XML-style prompt tags", () => {
    it("preserves non-HTML prompt tags as literal text", () => {
      const dom = parseHTML(
        renderMarkdown('<policy><rule importance="high">keep tags</rule></policy>'),
      );
      const p = dom.querySelector("p");
      expect(p).not.toBeNull();
      expect(p!.innerHTML).toContain("&lt;policy&gt;");
      expect(p!.innerHTML).toContain('&lt;rule importance="high"&gt;');
      expect(p!.innerHTML).toContain("&lt;/rule&gt;");
      expect(p!.innerHTML).toContain("&lt;/policy&gt;");
      expect(p!.textContent).toContain('<policy><rule importance="high">keep tags</rule></policy>');
    });

    it("keeps standard HTML on the sanitize path", () => {
      const dom = parseHTML(renderMarkdown('<img src=x onerror="alert(1)">'));
      const img = dom.querySelector("img");
      expect(img).not.toBeNull();
      expect(img!.hasAttribute("onerror")).toBe(false);
    });

    it("does not escape custom tags inside inline code spans", () => {
      const dom = parseHTML(renderMarkdown("`<policy>keep tags</policy>`"));
      const code = dom.querySelector("p > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<policy>keep tags</policy>");
    });

    it("does not escape custom tags inside fenced code blocks", () => {
      const dom = parseHTML(renderMarkdown("```\n<policy>keep tags</policy>\n```"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<policy>keep tags</policy>\n");
    });

    it("keeps fence-like lines inside a fenced code block", () => {
      const source = "```\n```not-a-closer\n<policy>\n# heading\n</policy>\n```";
      const omitted = renderMarkdown(source);
      const enabled = renderMarkdown(source, {
        renderUnknownXmlBlocksAsPreformatted: true,
      });

      expect(enabled).toBe(omitted);
      expect(parseHTML(enabled).querySelectorAll("pre > code")).toHaveLength(1);
      expect(parseHTML(enabled).querySelector("pre > code")?.textContent).toContain(
        "<policy>\n# heading\n</policy>",
      );
    });

    it("does not treat backticks in a fence info string as an opening fence", () => {
      const source = "```bad`info\n<policy>\n# heading\n</policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );

      expect(dom.querySelector("pre > code")?.textContent).toBe(
        "<policy>\n# heading\n</policy>\n",
      );
    });

    it("normalizes CRLF before matching complete blocks", () => {
      const dom = parseHTML(
        renderMarkdown("<policy>\r\n# heading\r\n</policy>", {
          renderUnknownXmlBlocksAsPreformatted: true,
        }),
      );

      expect(dom.querySelector("pre > code")?.textContent).toBe(
        "<policy>\n# heading\n</policy>\n",
      );
    });

    it("keeps markdown angle autolinks intact", () => {
      const dom = parseHTML(renderMarkdown("<https://example.com>"));
      const link = dom.querySelector("p > a");
      expect(link).not.toBeNull();
      expect(link!.textContent).toBe("https://example.com");
      expect(link!.getAttribute("href")).toBe("https://example.com");
    });

    it("preserves namespaced prompt tags as literal text", () => {
      const dom = parseHTML(renderMarkdown("<foo:bar>keep tags</foo:bar>"));
      const p = dom.querySelector("p");
      expect(p).not.toBeNull();
      expect(p!.innerHTML).toContain("&lt;foo:bar&gt;");
      expect(p!.innerHTML).toContain("&lt;/foo:bar&gt;");
      expect(p!.textContent).toContain("<foo:bar>keep tags</foo:bar>");
    });

    it("keeps markdown links with custom-tag labels clickable", () => {
      const dom = parseHTML(renderMarkdown("[<policy>read</policy>](https://example.com)"));
      const link = dom.querySelector("p > a");
      expect(link).not.toBeNull();
      expect(link!.getAttribute("href")).toBe("https://example.com");
      expect(link!.textContent).toBe("<policy>read</policy>");
    });

    it("preserves custom tags inside GFM table cells", () => {
      const dom = parseHTML(renderMarkdown("| A |\n| --- |\n| <policy>keep tags</policy> |"));
      const cell = dom.querySelector("tbody td");
      expect(cell).not.toBeNull();
      expect(cell!.innerHTML).toContain("&lt;policy&gt;");
      expect(cell!.innerHTML).toContain("&lt;/policy&gt;");
      expect(cell!.textContent).toBe("<policy>keep tags</policy>");
    });

    it("renders a complete unknown block as one preformatted code block when enabled", () => {
      const source = "<policy>\n# heading\n\n  indented body\n</policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe(`${source}\n`);
      expect(dom.querySelector("h1")).toBeNull();
      expect(dom.querySelector("ul")).toBeNull();
    });

    it("renders the issue fixture as one literal code block when enabled", () => {
      const dom = parseHTML(
        renderMarkdown(issueReproductionFixture, {
          renderUnknownXmlBlocksAsPreformatted: true,
        }),
      );
      const codes = dom.querySelectorAll("pre > code");
      expect(codes).toHaveLength(1);
      expect(codes[0]!.textContent).toContain("<current_file_diff>");
      expect(codes[0]!.textContent).toContain("-    # FIXME: replace this");
      expect(codes[0]!.textContent).toContain("+    new_value");
      expect(codes[0]!.textContent).toContain("</current_file_diff>");
      expect(dom.querySelector("h1")).toBeNull();
      expect(dom.querySelector("ul")).toBeNull();
    });

    it("renders each complete unknown block when several follow one another", () => {
      const dom = parseHTML(
        renderMarkdown(
          "<first-policy>\n# first\n</first-policy>\n\n<second-policy>\n# second\n</second-policy>",
          { renderUnknownXmlBlocksAsPreformatted: true },
        ),
      );
      const codes = dom.querySelectorAll("pre > code");

      expect(codes).toHaveLength(2);
      expect(codes[0]!.textContent).toContain("<first-policy>");
      expect(codes[1]!.textContent).toContain("<second-policy>");
      expect(dom.querySelectorAll("h1")).toHaveLength(0);
    });

    it("keeps the current Markdown behavior when the mode is omitted or false", () => {
      const omitted = parseHTML(renderMarkdown(issueReproductionFixture));
      const explicitFalse = parseHTML(
        renderMarkdown(issueReproductionFixture, {
          renderUnknownXmlBlocksAsPreformatted: false,
        }),
      );

      expect(omitted.querySelector("h1")).not.toBeNull();
      expect(omitted.querySelector("ul")).not.toBeNull();
      expect(explicitFalse.innerHTML).toBe(omitted.innerHTML);
    });

    it("leaves incomplete and mismatched blocks on the escaped Markdown path", () => {
      for (const source of [
        "<policy>\n# heading",
        "<policy>body</other>",
        "<policy>\n<rule>\n# heading\n</policy>\n</rule>",
        "Intro\n<policy>\n<rule>\n# heading\n</policy>\n</rule>",
        "Intro\n<policy>\n<rule>\n# heading\n</rule>\n</wrong>\n</policy>\nAfter",
        "<policy />",
      ]) {
        const dom = parseHTML(
          renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
        );
        expect(dom.querySelector("pre > code")).toBeNull();
        expect(dom.textContent).toContain(source.split("\n")[0]!);
      }
    });

    it("leaves an unmatched block after prose on the current path", () => {
      const source = "Hello **world**\n<policy>\n# heading\nmore prose";
      const omitted = renderMarkdown(source);
      const enabled = renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true });

      expect(enabled).toBe(omitted);
      expect(parseHTML(enabled).querySelector("h1")).not.toBeNull();
    });

    it("preserves prose before a complete block", () => {
      const source = "Intro **text**\n<policy>\n# heading\n</policy>\nAfter";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );

      expect(dom.querySelector("p")?.textContent).toContain("Intro text");
      expect(dom.querySelector("pre > code")?.textContent).toBe(
        "<policy>\n# heading\n</policy>\n",
      );
      expect(dom.textContent).toContain("After");
      expect(dom.querySelector("h1")).toBeNull();
    });

    it("preserves indentation when a complete block follows prose", () => {
      const source = "Intro\n  <policy>\n# heading\n  </policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );

      expect(dom.querySelector("p")?.textContent).toContain("Intro");
      expect(dom.querySelector("pre > code")?.textContent).toBe(
        "  <policy>\n# heading\n  </policy>\n",
      );
      expect(dom.querySelector("h1")).toBeNull();
    });

    it("captures complete blocks inside list and blockquote items", () => {
      for (const source of [
        "- <policy>\n  # heading\n  </policy>",
        "> <policy>\n> # heading\n> </policy>",
      ]) {
        const dom = parseHTML(
          renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
        );

        expect(dom.querySelector("pre > code")?.textContent).toContain("<policy>");
        expect(dom.querySelector("h1")).toBeNull();
      }
    });

    it("rescans each transformed list item independently", () => {
      const source = "- Intro <policy>\n  # heading\n  </policy>\n- <policy>\n  # heading\n  </policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );

      expect(dom.querySelectorAll("pre > code")).toHaveLength(1);
    });

    it("captures complete blocks with up to three leading spaces", () => {
      const source = "  <policy>\n# heading\n  </policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );

      expect(dom.querySelector("pre > code")?.textContent).toBe(`${source}\n`);
      expect(dom.querySelector("h1")).toBeNull();
    });

    it("balances nested blocks with the same tag name", () => {
      const source = "<example>\n1\n<example>\ninner\n</example>\ntail\n</example>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );
      const code = dom.querySelector("pre > code");

      expect(code).not.toBeNull();
      expect(code!.textContent).toBe(`${source}\n`);
      expect(dom.querySelector("p")).toBeNull();
    });

    it("keeps internal whitespace inside the captured block", () => {
      const source = "<policy>\n\n    first\n\n      second\n\n</policy>";
      const dom = parseHTML(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      );
      expect(dom.querySelector("pre > code")!.textContent).toBe(`${source}\n`);
    });

    it("uses separate cache entries for the two rendering modes", () => {
      const source = "<cache-policy>\n# cached heading\n</cache-policy>";
      const defaultHtml = renderMarkdown(source, {
        renderUnknownXmlBlocksAsPreformatted: false,
      });
      const preformattedHtml = renderMarkdown(source, {
        renderUnknownXmlBlocksAsPreformatted: true,
      });

      expect(renderMarkdown(source)).toBe(defaultHtml);
      expect(
        renderMarkdown(source, { renderUnknownXmlBlocksAsPreformatted: true }),
      ).toBe(preformattedHtml);
      expect(preformattedHtml).not.toBe(defaultHtml);

      const reverseSource = "<reverse-cache-policy>\n# reverse heading\n</reverse-cache-policy>";
      const reversePreformatted = renderMarkdown(reverseSource, {
        renderUnknownXmlBlocksAsPreformatted: true,
      });
      const reverseDefault = renderMarkdown(reverseSource, {
        renderUnknownXmlBlocksAsPreformatted: false,
      });

      expect(renderMarkdown(reverseSource, { renderUnknownXmlBlocksAsPreformatted: true })).toBe(
        reversePreformatted,
      );
      expect(renderMarkdown(reverseSource)).toBe(reverseDefault);
      expect(reversePreformatted).not.toBe(reverseDefault);
    });

    it("preserves protected Markdown paths when enabled", () => {
      const dom = parseHTML(
        renderMarkdown(
          '<img src=x onerror="alert(1)">\n\n<bash-input>echo hi</bash-input>\n\n`<policy>inline</policy>`\n\n```\n<policy>fenced</policy>\n```',
          { renderUnknownXmlBlocksAsPreformatted: true },
        ),
      );
      expect(dom.querySelector("img")!.hasAttribute("onerror")).toBe(false);
      expect(dom.querySelector("p > code")!.textContent).toBe("<policy>inline</policy>");
      expect(dom.querySelectorAll("pre > code")[0]!.textContent).toBe("!echo hi\n");
      expect(dom.querySelectorAll("pre > code")[1]!.textContent).toBe(
        "<policy>fenced</policy>\n",
      );
    });

    it("does not capture unknown tags inside multiline inline code or known HTML", () => {
      for (const source of [
        "prefix `\n<policy>\n# heading\n</policy>\n` suffix",
        "Intro <span>\n<policy>\n# heading\n</policy>\n</span>",
      ]) {
        const omitted = renderMarkdown(source);
        const enabled = renderMarkdown(source, {
          renderUnknownXmlBlocksAsPreformatted: true,
        });

        expect(enabled).toBe(omitted);
        expect(parseHTML(enabled).querySelector("pre > code")).toBeNull();
      }
    });
  });

  describe("Claude Code shell shortcuts", () => {
    it("renders <bash-input> as a shell code block with ! prefix", () => {
      const dom = parseHTML(renderMarkdown("<bash-input>git pull origin main</bash-input>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("!git pull origin main\n");
      // Tag itself must not survive in the output.
      expect(dom.innerHTML).not.toMatch(/<\/?bash-input>/);
    });

    it("preserves multi-line commands in <bash-input>", () => {
      const dom = parseHTML(renderMarkdown("<bash-input>cd /tmp\nls -la</bash-input>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("!cd /tmp\nls -la\n");
    });

    it("renders <bash-stdout> as an unlabelled code block", () => {
      const dom = parseHTML(renderMarkdown("<bash-stdout>hello world</bash-stdout>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("hello world\n");
      expect(dom.innerHTML).not.toMatch(/<\/?bash-stdout>/);
    });

    it("renders <bash-stderr> as an unlabelled code block", () => {
      const dom = parseHTML(renderMarkdown("<bash-stderr>oops</bash-stderr>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("oops\n");
      expect(dom.innerHTML).not.toMatch(/<\/?bash-stderr>/);
    });

    it("drops empty <bash-stdout> and <bash-stderr> blocks", () => {
      const dom = parseHTML(
        renderMarkdown(
          "<bash-input>true</bash-input>" +
            "<bash-stdout></bash-stdout>" +
            "<bash-stderr></bash-stderr>",
        ),
      );
      const codes = dom.querySelectorAll("pre > code");
      expect(codes.length).toBe(1);
      expect(codes[0]!.textContent).toBe("!true\n");
    });

    it("handles input with backticks by picking a longer fence", () => {
      const dom = parseHTML(
        renderMarkdown("<bash-input>echo ```triple``` and ` single`</bash-input>"),
      );
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("!echo ```triple``` and ` single`\n");
    });

    it("handles consecutive input/stdout pair", () => {
      const dom = parseHTML(
        renderMarkdown("<bash-input>echo hi</bash-input>" + "<bash-stdout>hi\n</bash-stdout>"),
      );
      const codes = dom.querySelectorAll("pre > code");
      expect(codes.length).toBe(2);
      expect(codes[0]!.textContent).toBe("!echo hi\n");
      expect(codes[1]!.textContent).toBe("hi\n");
    });

    it("leaves wrappers inside fenced code blocks alone", () => {
      // The user is talking ABOUT the tag, not invoking one. The
      // marked extension runs at the lexer level, so once the
      // fenced block consumes these characters they are never
      // re-tokenized as wrappers.
      const dom = parseHTML(renderMarkdown("```\n<bash-input>echo hi</bash-input>\n```"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<bash-input>echo hi</bash-input>\n");
    });

    it("leaves wrappers inside indented code blocks alone", () => {
      const dom = parseHTML(renderMarkdown("    <bash-input>echo hi</bash-input>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<bash-input>echo hi</bash-input>\n");
    });

    it("leaves custom tags inside longer-closing fences alone", () => {
      const dom = parseHTML(renderMarkdown("~~~\n<policy>keep tags</policy>\n~~~~"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<policy>keep tags</policy>\n");
    });

    it("leaves custom tags inside unclosed fences alone", () => {
      const dom = parseHTML(renderMarkdown("~~~\n<policy>keep tags</policy>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<policy>keep tags</policy>\n");
    });

    it("tags the input block with language-shell", () => {
      const html = renderMarkdown("<bash-input>echo hi</bash-input>");
      expect(html).toMatch(/<code[^>]*class="language-shell"/);
    });

    it("preserves leading whitespace and indentation in stdout", () => {
      // Shell output frequently has indentation that's meaningful
      // (tree output, table layouts, log-line columns). Trimming
      // would corrupt the transcript.
      const dom = parseHTML(
        renderMarkdown("<bash-stdout>  line one\n    nested\n  line two</bash-stdout>"),
      );
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("  line one\n    nested\n  line two\n");
    });

    it("preserves leading and trailing blank lines in stdout", () => {
      const dom = parseHTML(renderMarkdown("<bash-stdout>\n\nbody\n\n</bash-stdout>"));
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      // marked normalizes the final newline but leading blanks
      // and the interior blank-line structure are preserved.
      expect(code!.textContent).toMatch(/^\n\nbody\n/);
    });

    it("leaves custom tags inside bash output literal", () => {
      const dom = parseHTML(
        renderMarkdown("<bash-stdout><policy>keep tags</policy></bash-stdout>"),
      );
      const code = dom.querySelector("pre > code");
      expect(code).not.toBeNull();
      expect(code!.textContent).toBe("<policy>keep tags</policy>\n");
    });
  });

  describe("edge cases", () => {
    it("returns empty string for empty input", () => {
      expect(renderMarkdown("")).toBe("");
    });

    it("passes through plain text", () => {
      const dom = parseHTML(renderMarkdown("just plain text"));
      const p = dom.querySelector("p");
      expect(p).not.toBeNull();
      expect(p!.textContent).toBe("just plain text");
    });

    it("removes trailing newlines to prevent extra height", () => {
      const dom = parseHTML(renderMarkdown("text\n\n"));
      const p = dom.querySelector("p");
      expect(p).not.toBeNull();
      expect(p!.textContent).toBe("text");
    });
  });
});
