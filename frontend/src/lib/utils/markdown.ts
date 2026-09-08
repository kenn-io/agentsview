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
const XML_TAG_SCAN_RE = new RegExp(XML_TAG_ESCAPE_RE.source, "g");

type MarkdownToken = Token & Record<string, unknown>;

type MarkdownFenceRange = { start: number; end: number };

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

function findFirstCompleteUnknownXmlBlock(
  src: string,
): { start: number; rawStart: number; end: number } | undefined {
  const fenceRanges: MarkdownFenceRange[] = [];
  const fenceRe = /^ {0,3}(`{3,}|~{3,})[^\n]*$/gm;
  let openFence: { start: number; char: string; length: number } | undefined;
  let fence: RegExpExecArray | null;
  while ((fence = fenceRe.exec(src)) !== null) {
    const marker = fence[1]!;
    if (!openFence) {
      openFence = { start: fence.index, char: marker[0]!, length: marker.length };
    } else if (marker[0] === openFence.char && marker.length >= openFence.length) {
      fenceRanges.push({ start: openFence.start, end: fence.index + fence[0].length });
      openFence = undefined;
    }
  }
  if (openFence) fenceRanges.push({ start: openFence.start, end: src.length });

  const protectedRanges = [...fenceRanges];
  const inlineCodeRe = /`+/g;
  let openInlineCode: { start: number; length: number } | undefined;
  let fenceIndex = 0;
  let inlineCode: RegExpExecArray | null;
  while ((inlineCode = inlineCodeRe.exec(src)) !== null) {
    while (fenceIndex < fenceRanges.length && inlineCode.index >= fenceRanges[fenceIndex]!.end) {
      fenceIndex += 1;
    }
    const fenceRange = fenceRanges[fenceIndex];
    const inFence = fenceRange !== undefined && inlineCode.index >= fenceRange.start;
    if (inFence) continue;

    if (!openInlineCode) {
      openInlineCode = { start: inlineCode.index, length: inlineCode[0].length };
    } else if (inlineCode[0].length === openInlineCode.length) {
      protectedRanges.push({ start: openInlineCode.start, end: inlineCode.index + inlineCode[0].length });
      openInlineCode = undefined;
    }
  }
  protectedRanges.sort((left, right) => left.start - right.start);

  const openUnknownTags: Array<{ name: string; start?: number; rawStart?: number }> = [];
  let malformedUnknownTags: string[] = [];
  const openHtmlTags: string[] = [];
  let firstComplete: { start: number; rawStart: number; end: number } | undefined;
  let protectedIndex = 0;
  let lineStart = 0;
  let scanCursor = 0;

  XML_TAG_SCAN_RE.lastIndex = 0;
  let tag: RegExpExecArray | null;
  while ((tag = XML_TAG_SCAN_RE.exec(src)) !== null) {
    const between = src.slice(scanCursor, tag.index);
    const newline = between.lastIndexOf("\n");
    if (newline >= 0) lineStart = scanCursor + newline + 1;
    scanCursor = XML_TAG_SCAN_RE.lastIndex;

    while (protectedIndex < protectedRanges.length && tag.index >= protectedRanges[protectedIndex]!.end) {
      protectedIndex += 1;
    }
    const protectedRange = protectedRanges[protectedIndex];
    if (protectedRange && tag.index >= protectedRange.start) continue;

    const name = tag[1]?.toLowerCase();
    if (!name) continue;

    const tagText = tag[0];
    const closing = tagText.startsWith("</");
    const selfClosing = /\/\s*>$/.test(tagText);
    if (isPreservedHtmlTag(name)) {
      if (closing) {
        if (openHtmlTags.at(-1) === name) openHtmlTags.pop();
      } else if (!selfClosing && !VOID_HTML_TAGS.has(name)) {
        openHtmlTags.push(name);
      }
      continue;
    }

    if (openHtmlTags.length > 0) continue;

    if (tagText.startsWith("</")) {
      if (malformedUnknownTags.length > 0) {
        const malformedIndex = malformedUnknownTags.lastIndexOf(name);
        if (malformedIndex >= 0) malformedUnknownTags.splice(malformedIndex, 1);
        continue;
      }
      const opening = openUnknownTags.at(-1);
      if (!opening) continue;
      if (opening.name !== name) {
        malformedUnknownTags = openUnknownTags.map((open) => open.name);
        openUnknownTags.length = 0;
        const malformedIndex = malformedUnknownTags.lastIndexOf(name);
        if (malformedIndex >= 0) malformedUnknownTags.splice(malformedIndex, 1);
        continue;
      }
      openUnknownTags.pop();
      if (opening.start !== undefined && opening.rawStart !== undefined) {
        const candidate = {
          start: opening.start,
          rawStart: opening.rawStart,
          end: XML_TAG_SCAN_RE.lastIndex,
        };
        if (!firstComplete || candidate.start < firstComplete.start) {
          firstComplete = candidate;
        }
      }
      continue;
    }

    if (selfClosing || openHtmlTags.length > 0) continue;
    if (malformedUnknownTags.length > 0) {
      malformedUnknownTags.push(name);
      continue;
    }
    const indentation = src.slice(lineStart, tag.index);
    const start = /^ {0,3}$/.test(indentation)
      ? tag.index
      : undefined;
    openUnknownTags.push({
      name,
      start,
      rawStart: start === undefined ? undefined : lineStart,
    });
  }

  return firstComplete;
}

/** Build a tokenizer that captures a complete unknown XML block before
 *  marked can parse Markdown in its body. Line-start matching keeps inline
 *  code and prose containing the same tags on their existing paths. */
function unknownXmlBlockExtension(): TokenizerExtension {
  return {
    name: "unknownXmlBlock",
    level: "block",
    start(src) {
      const block = findFirstCompleteUnknownXmlBlock(src);
      return block && block.rawStart > 0 ? block.rawStart : undefined;
    },
    tokenizer(src) {
      const block = findFirstCompleteUnknownXmlBlock(src);
      if (!block || block.rawStart !== 0) return undefined;
      const raw = src.slice(block.rawStart, block.end);
      return {
        type: "code",
        raw,
        text: raw,
      };
    },
  };
}

function createParser(renderUnknownXmlBlocksAsPreformatted: boolean): Marked {
  const instance = new Marked({
    gfm: true,
    breaks: true,
  });

  instance.use({
    extensions: [
      ...(renderUnknownXmlBlocksAsPreformatted ? [unknownXmlBlockExtension()] : []),
      bashWrapperExtension("bashInput", "bash-input", "!", "shell"),
      bashWrapperExtension("bashStdout", "bash-stdout", "", ""),
      bashWrapperExtension("bashStderr", "bash-stderr", "", ""),
    ],
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

  const markdownParser = renderUnknownXmlBlocksAsPreformatted ? preformattedParser : parser;
  const resolved = escapeCustomXmlTags(resolveAssetURLs(text), markdownParser);
  const html = markdownParser.parser(resolved) as string;
  const safe = DOMPurify.sanitize(html);

  const entry: RenderCacheEntry = cache.get(text) ?? [undefined, undefined];
  entry[modeIndex] = safe;
  cache.set(text, entry);
  return safe;
}
