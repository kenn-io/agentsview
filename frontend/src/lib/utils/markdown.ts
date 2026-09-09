// kit-ui-check-ignore: app renderer adds agent-specific XML escaping and shell wrapper tags on top of marked; migrating to kit-ui createMarkdownRenderer needs a dedicated behavior-preserving pass.
import {
  Marked,
  Tokenizer,
  type Token,
  type TokenizerExtension,
} from "marked";
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

const VOID_HTML_TAGS = new Set([
  "area",
  "base",
  "br",
  "col",
  "embed",
  "hr",
  "img",
  "input",
  "link",
  "meta",
  "param",
  "source",
  "track",
  "wbr",
]);

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

function isSelfClosingTag(tagText: string): boolean {
  return /\/\s*>$/.test(tagText);
}

function tagAtLineStart(src: string, offset: number): RegExpExecArray | undefined {
  const match = new RegExp(`^ {0,3}${XML_TAG_ESCAPE_RE.source}`).exec(src.slice(offset));
  return match ?? undefined;
}

function matchUnknownXmlBlockAt(src: string, offset: number): number | undefined {
  if (offset < 0 || offset >= src.length || (offset > 0 && src[offset - 1] !== "\n")) {
    return undefined;
  }

  const opening = tagAtLineStart(src, offset);
  if (!opening) return undefined;

  const openingText = opening[0];
  const openingName = opening[1]?.toLowerCase();
  if (
    !openingName ||
    openingText.startsWith("</") ||
    isSelfClosingTag(openingText) ||
    isPreservedHtmlTag(openingName)
  ) {
    return undefined;
  }

  const stack = [openingName];
  const openHtmlTags: string[] = [];
  const tags = new RegExp(XML_TAG_ESCAPE_RE.source, "g");
  tags.lastIndex = offset + openingText.length;
  let tag: RegExpExecArray | null;
  while ((tag = tags.exec(src)) !== null) {
    const tagText = tag[0];
    const name = tag[1]?.toLowerCase();
    if (!name) continue;

    const closing = tagText.startsWith("</");
    const selfClosing = isSelfClosingTag(tagText);
    if (isPreservedHtmlTag(name)) {
      if (closing) {
        if (openHtmlTags.at(-1) === name) openHtmlTags.pop();
      } else if (!selfClosing && !VOID_HTML_TAGS.has(name)) {
        openHtmlTags.push(name);
      }
      continue;
    }

    if (openHtmlTags.length > 0 || selfClosing) continue;
    if (closing) {
      if (stack.at(-1) !== name) return undefined;
      stack.pop();
      if (stack.length === 0) return tags.lastIndex;
      continue;
    }

    stack.push(name);
  }

  return undefined;
}

type TextRange = { start: number; end: number };

function closedBacktickRanges(src: string, end: number): TextRange[] {
  const runs = new Map<number, number[]>();
  const re = /`+/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(src)) !== null && match.index < end) {
    const length = match[0].length;
    const starts = runs.get(length) ?? [];
    starts.push(match.index);
    runs.set(length, starts);
  }

  const ranges: TextRange[] = [];
  for (const [length, starts] of runs) {
    for (let index = 0; index + 1 < starts.length; index += 2) {
      const start = starts[index]!;
      const close = starts[index + 1]!;
      ranges.push({ start, end: Math.min(close + length, end) });
    }
  }
  return ranges;
}

function isInRange(offset: number, ranges: TextRange[]): boolean {
  return ranges.some((range) => offset >= range.start && offset < range.end);
}

function updateKnownHtmlTags(stack: string[], tagText: string, name: string): void {
  if (!isPreservedHtmlTag(name)) return;
  if (tagText.startsWith("</")) {
    if (stack.at(-1) === name) stack.pop();
  } else if (!isSelfClosingTag(tagText) && !VOID_HTML_TAGS.has(name)) {
    stack.push(name);
  }
}

function findUnknownXmlCandidate(src: string): number | undefined {
  if (src.indexOf("<") < 0) return undefined;

  const blankLine = /(?:^|\n)[ \t]*(?:\n|$)/.exec(src);
  const windowEnd = blankLine?.index ?? src.length;
  const protectedRanges = closedBacktickRanges(src, windowEnd);
  const openHtmlTags: string[] = [];
  const tags = new RegExp(XML_TAG_ESCAPE_RE.source, "g");
  let tagCursor = 0;

  for (let lineStart = 0; lineStart < windowEnd; ) {
    while (true) {
      tags.lastIndex = tagCursor;
      const tag = tags.exec(src);
      if (!tag || tag.index >= lineStart) break;
      tagCursor = tags.lastIndex;
      if (!isInRange(tag.index, protectedRanges)) {
        const name = tag[1]?.toLowerCase();
        if (name) updateKnownHtmlTags(openHtmlTags, tag[0], name);
      }
    }

    if (!isInRange(lineStart, protectedRanges)) {
      const lineTag = tagAtLineStart(src, lineStart);
      if (lineTag) {
        const tagText = lineTag[0];
        const name = lineTag[1]?.toLowerCase();
        if (name && isPreservedHtmlTag(name)) {
          updateKnownHtmlTags(openHtmlTags, tagText, name);
          tagCursor = lineStart + tagText.indexOf("<") + tagText.length;
        } else if (openHtmlTags.length === 0 && name && !tagText.startsWith("</") && !isSelfClosingTag(tagText)) {
          const end = matchUnknownXmlBlockAt(src, lineStart);
          return end === undefined ? undefined : lineStart;
        } else if (lineTag) {
          tagCursor = lineStart + tagText.indexOf("<") + tagText.length;
        }
      }
    }

    const nextNewline = src.indexOf("\n", lineStart);
    if (nextNewline < 0 || nextNewline + 1 >= windowEnd) break;
    lineStart = nextNewline + 1;
  }

  return undefined;
}

function unknownXmlBlockExtension(): TokenizerExtension {
  return {
    name: "unknownXmlBlock",
    level: "block",
    start(src) {
      const candidate = findUnknownXmlCandidate(src);
      return candidate === 0 ? undefined : candidate;
    },
    tokenizer(src) {
      const end = matchUnknownXmlBlockAt(src, 0);
      if (end === undefined) return undefined;
      const raw = src.slice(0, end);
      return { type: "code", raw, text: raw };
    },
  };
}

function unknownXmlHtmlBoundary() {
  return {
    html(src: string) {
      if (/^<(?:!|\?|script\b|pre\b|style\b|textarea\b)/i.test(src)) return false;
      const candidate = findUnknownXmlCandidate(src);
      if (candidate === undefined || candidate === 0) return false;

      const token = Tokenizer.prototype.html.call(this, src);
      if (!token || token.raw.length <= candidate) return token;
      const raw = token.raw.slice(0, candidate);
      return { ...token, raw, text: raw };
    },
  };
}

function createParser(
  renderUnknownXmlBlocksAsPreformatted: boolean,
): Marked {
  const instance = new Marked({
    gfm: true,
    breaks: true,
  });

  instance.use({
    extensions: [
      ...(renderUnknownXmlBlocksAsPreformatted
        ? [unknownXmlBlockExtension()]
        : []),
      bashWrapperExtension("bashInput", "bash-input", "!", "shell"),
      bashWrapperExtension("bashStdout", "bash-stdout", "", ""),
      bashWrapperExtension("bashStderr", "bash-stderr", "", ""),
    ],
    ...(renderUnknownXmlBlocksAsPreformatted
      ? { tokenizer: unknownXmlHtmlBoundary() }
      : {}),
  });

  return instance;
}

const parser = createParser(false);
const preformattedParser = createParser(true);

type RenderCacheEntry = [string | undefined, string | undefined];

const cache = new LRUCache<string, RenderCacheEntry>(6000);

function getApiBase(): string {
  const baseEl = document.querySelector("base[href]");
  if (baseEl) {
    const base = new URL(document.baseURI).pathname.replace(/\/$/, "");
    return `${base}/api/v1`;
  }
  return "/api/v1";
}

function resolveAssetURLs(text: string): string {
  return text.replace(/asset:\/\/([^\s)]+)/g, `${getApiBase()}/assets/$1`);
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
    value && typeof value === "object" && "type" in (value as Record<string, unknown>),
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

function escapeCustomXmlTags(text: string, markdownParser: Marked): MarkdownToken[] {
  const tokens = markdownParser.lexer(text.trimEnd()) as MarkdownToken[];
  return escapeCustomXmlTokens(tokens);
}

export interface MarkdownRenderOptions {
  renderUnknownXmlBlocksAsPreformatted?: boolean;
}

export function renderMarkdown(text: string, options: MarkdownRenderOptions = {}): string {
  if (!text) return "";

  const renderUnknownXmlBlocksAsPreformatted =
    options.renderUnknownXmlBlocksAsPreformatted === true;
  const modeIndex = renderUnknownXmlBlocksAsPreformatted ? 1 : 0;
  const cached = cache.get(text)?.[modeIndex];
  if (cached !== undefined) return cached;

  const resolvedText = resolveAssetURLs(text);
  const markdownParser = renderUnknownXmlBlocksAsPreformatted ? preformattedParser : parser;
  const resolved = escapeCustomXmlTags(resolvedText, markdownParser);
  const html = markdownParser.parser(resolved) as string;
  const safe = DOMPurify.sanitize(html);

  const entry: RenderCacheEntry = cache.get(text) ?? [undefined, undefined];
  entry[modeIndex] = safe;
  cache.set(text, entry);
  return safe;
}
