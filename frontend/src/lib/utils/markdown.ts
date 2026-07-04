// kit-ui-check-ignore: app renderer adds agent-specific XML escaping and shell wrapper tags on top of marked; migrating to kit-ui createMarkdownRenderer needs a dedicated behavior-preserving pass.
import { Marked, type Token, type TokenizerExtension } from "marked";
// kit-ui-check-ignore: app renderer sanitizes the custom marked output above; migrating to kit-ui createMarkdownRenderer needs a dedicated behavior-preserving pass.
import DOMPurify from "dompurify";
import { LRUCache } from "./cache.js";

const KNOWN_HTML_TAGS = new Set([
  "a",
  "abbr",
  "address",
  "area",
  "article",
  "aside",
  "audio",
  "b",
  "base",
  "bdi",
  "bdo",
  "blockquote",
  "body",
  "br",
  "button",
  "canvas",
  "caption",
  "cite",
  "code",
  "col",
  "colgroup",
  "data",
  "datalist",
  "dd",
  "del",
  "details",
  "dfn",
  "dialog",
  "div",
  "dl",
  "dt",
  "em",
  "embed",
  "fieldset",
  "figcaption",
  "figure",
  "footer",
  "form",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "head",
  "header",
  "hgroup",
  "hr",
  "html",
  "i",
  "iframe",
  "img",
  "input",
  "ins",
  "kbd",
  "label",
  "legend",
  "li",
  "link",
  "main",
  "map",
  "mark",
  "menu",
  "meta",
  "meter",
  "nav",
  "noscript",
  "object",
  "ol",
  "optgroup",
  "option",
  "output",
  "p",
  "picture",
  "pre",
  "progress",
  "q",
  "rp",
  "rt",
  "ruby",
  "s",
  "samp",
  "script",
  "section",
  "select",
  "slot",
  "small",
  "source",
  "span",
  "strong",
  "style",
  "sub",
  "summary",
  "sup",
  "svg",
  "table",
  "tbody",
  "td",
  "template",
  "textarea",
  "tfoot",
  "th",
  "thead",
  "time",
  "title",
  "tr",
  "track",
  "u",
  "ul",
  "var",
  "video",
  "wbr",
]);

const XML_TAG_ESCAPE_RE = /<\/?([A-Za-z][A-Za-z0-9:_-]*)(?:"[^"]*"|'[^']*'|[^"'<>])*?>/g;

type MarkdownToken = Token & Record<string, unknown>;

/** Build a marked tokenizer extension that consumes a Claude Code
 *  shell-shortcut wrapper tag and emits a `code` token directly.
 *  Because this runs at the lexer level, occurrences of the tag
 *  inside markdown code blocks never reach the extension. */
function bashWrapperExtension(
  name: string,
  tag: string,
  prefix: string,
  lang: string,
): TokenizerExtension {
  const startRe = new RegExp(`<${tag}>`);
  const fullRe = new RegExp(`^<${tag}>([\\s\\S]*?)</${tag}>`);
  return {
    name,
    level: "block",
    start(src) {
      const m = startRe.exec(src);
      return m?.index;
    },
    tokenizer(src) {
      const m = fullRe.exec(src);
      if (!m) return undefined;
      const captured = m[1] ?? "";
      if (!captured.trim()) {
        return { type: "space", raw: m[0] };
      }
      return {
        type: "code",
        raw: m[0],
        lang,
        text: prefix + captured,
      };
    },
  };
}

const parser = new Marked({
  gfm: true,
  breaks: true,
});

parser.use({
  extensions: [
    bashWrapperExtension("bashInput", "bash-input", "!", "shell"),
    bashWrapperExtension("bashStdout", "bash-stdout", "", ""),
    bashWrapperExtension("bashStderr", "bash-stderr", "", ""),
  ],
});

const cache = new LRUCache<string, string>(6000);

function getApiBase(): string {
  const baseEl = document.querySelector("base[href]");
  if (baseEl) {
    const base = new URL(document.baseURI).pathname.replace(/\/$/, "");
    return `${base}/api/v1`;
  }
  return "/api/v1";
}

function resolveAssetURLs(text: string): string {
  return text.replace(
    /asset:\/\/([^\s)]+)/g,
    `${getApiBase()}/assets/$1`,
  );
}

function isPreservedHtmlTag(name: string): boolean {
  return (
    KNOWN_HTML_TAGS.has(name) ||
    name === "bash-input" ||
    name === "bash-stdout" ||
    name === "bash-stderr"
  );
}

function escapeTagBrackets(text: string): string {
  return text.replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function isProtectedAutolink(raw: string): boolean {
  const inner = raw.slice(1, -1);
  return (
    /^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(inner) ||
    /^mailto:/i.test(inner) ||
    /^[^\s<>@]+@[^\s<>]+$/.test(inner)
  );
}

function shouldEscapeCustomXmlLiteral(raw: string | undefined): boolean {
  if (!raw || isProtectedAutolink(raw)) {
    return false;
  }

  const match = XML_TAG_ESCAPE_RE.exec(raw);
  XML_TAG_ESCAPE_RE.lastIndex = 0;
  if (!match) {
    return false;
  }

  const name = match[1]?.toLowerCase() ?? "";
  return !isPreservedHtmlTag(name);
}

function toEscapedTextToken(raw: string): MarkdownToken {
  return {
    type: "text",
    raw,
    text: escapeTagBrackets(raw),
    escaped: true,
  };
}

function isMarkdownToken(value: unknown): value is MarkdownToken {
  return Boolean(
    value &&
      typeof value === "object" &&
      "type" in (value as Record<string, unknown>),
  );
}

function escapeTokenValue(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map((entry) => escapeTokenValue(entry));
  }
  if (isMarkdownToken(value)) {
    return escapeCustomXmlToken(value);
  }
  if (value && typeof value === "object") {
    const next = {
      ...(value as Record<string, unknown>),
    };
    for (const [key, entry] of Object.entries(next)) {
      next[key] = escapeTokenValue(entry);
    }
    return next;
  }
  return value;
}

function escapeCustomXmlToken(token: MarkdownToken): MarkdownToken {
  if (token.type === "html" && shouldEscapeCustomXmlLiteral(token.raw)) {
    return toEscapedTextToken(token.raw!);
  }

  if (
    token.type === "link" &&
    token.raw?.startsWith("<") &&
    shouldEscapeCustomXmlLiteral(token.raw)
  ) {
    return toEscapedTextToken(token.raw!);
  }

  const next: MarkdownToken = { ...token };
  for (const [key, value] of Object.entries(next)) {
    next[key] = escapeTokenValue(value);
  }

  return next;
}

function escapeCustomXmlTokens(tokens: MarkdownToken[]): MarkdownToken[] {
  return tokens.map((token) => escapeCustomXmlToken(token));
}

function escapeCustomXmlTags(text: string): MarkdownToken[] {
  const tokens = parser.lexer(text.trimEnd()) as MarkdownToken[];
  return escapeCustomXmlTokens(tokens);
}

export function renderMarkdown(text: string): string {
  if (!text) return "";

  const cached = cache.get(text);
  if (cached !== undefined) return cached;

  const resolved = escapeCustomXmlTags(resolveAssetURLs(text));
  const html = parser.parser(resolved) as string;
  const safe = DOMPurify.sanitize(html);

  cache.set(text, safe);
  return safe;
}
