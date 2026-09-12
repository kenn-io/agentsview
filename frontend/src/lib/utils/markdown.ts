// kit-ui-check-ignore: app renderer adds agent-specific XML escaping and shell wrapper tags on top of marked; migrating to kit-ui createMarkdownRenderer needs a dedicated behavior-preserving pass.
import { Lexer, Marked, Tokenizer, type Links, type Token, type TokenizerExtension, type TokenizerAndRendererExtension } from "marked";
// kit-ui-check-ignore: app renderer sanitizes the custom marked output above; migrating to kit-ui createMarkdownRenderer needs a dedicated behavior-preserving pass.
import DOMPurify from "dompurify";
import { LRUCache } from "./cache.js";
import { escapeHTML } from "./highlight.js";
import { authHeaders, getBase } from "../api/runtime.js";

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

function tagAtLineStart(
  src: string,
  offset: number,
  end = src.length,
): RegExpExecArray | undefined {
  let tagOffset = offset;
  while (tagOffset < Math.min(offset + 3, end) && src[tagOffset] === " ") {
    tagOffset += 1;
  }
  if (src[tagOffset] !== "<") return undefined;

  const tagEnd = tagInputEnd(src, tagOffset, end);
  const match = new RegExp(`^ {0,3}${XML_TAG_ESCAPE_RE.source}`).exec(
    src.slice(offset, tagEnd),
  );
  return match ?? undefined;
}

type ScannerToken =
  | { kind: "protected"; start: number; end: number }
  | { kind: "ignored"; start: number; end: number }
  | { kind: "tag"; start: number; end: number; raw: string };

type MarkdownScanner = {
  tokenizer: Tokenizer;
  links: Links;
  ignoredFenceRange?: { start: number; end: number };
};

const UNKNOWN_XML_SCAN_CONTEXT = Symbol("unknownXmlScanContext");
const UNKNOWN_XML_LEXER_LINKS = Symbol("unknownXmlLexerLinks");

type UnknownXmlScanContext = {
  source: string;
  results: Map<number, number | undefined>;
  candidateResults: Map<number, number | undefined>;
  links: Links;
};

type TokenArrayWithScanContext = unknown[] & {
  [UNKNOWN_XML_SCAN_CONTEXT]?: UnknownXmlScanContext;
};

function getUnknownXmlScanContext(
  tokens: unknown,
  src: string,
  links?: Links,
): { context: UnknownXmlScanContext; offset: number } | undefined {
  if (!Array.isArray(tokens)) return undefined;
  const tokenArray = tokens as TokenArrayWithScanContext;
  let context = tokenArray[UNKNOWN_XML_SCAN_CONTEXT];
  const offset = context ? context.source.length - src.length : 0;
  if (!context || offset < 0 || !context.source.startsWith(src, offset)) {
    const documentLinks = links ?? collectReferenceLinks(src);
    context = {
      source: src,
      results: new Map(),
      candidateResults: new Map(),
      links: Object.assign(Object.create(null), links, documentLinks),
    };
    Object.defineProperty(tokenArray, UNKNOWN_XML_SCAN_CONTEXT, {
      configurable: true,
      value: context,
    });
    return { context, offset: 0 };
  }
  return { context, offset };
}

function collectReferenceLinks(src: string): Links {
  if (!src.includes("[")) return Object.create(null) as Links;

  const tokens = new Lexer({ gfm: true, breaks: true }).lex(src) as Token[] & {
    links?: Links;
  };
  return tokens.links ?? (Object.create(null) as Links);
}

function createMarkdownScanner(links: Links = Object.create(null)): MarkdownScanner {
  const lexer = new Lexer({ gfm: true, breaks: true }) as unknown as {
    tokenizer: Tokenizer;
  };
  return { tokenizer: lexer.tokenizer, links };
}

function getLexerTokens(value: unknown): unknown {
  if (!value || typeof value !== "object") return undefined;
  const lexer = (value as { lexer?: { tokens?: unknown } }).lexer;
  return lexer?.tokens;
}

function getLexerLinks(value: unknown, src?: string): Links | undefined {
  if (!value || typeof value !== "object") return undefined;
  const lexer = (value as { lexer?: object }).lexer;
  if (!lexer) return undefined;

  const tokens = (lexer as { tokens?: unknown }).tokens;
  if (Array.isArray(tokens)) {
    const context = (tokens as TokenArrayWithScanContext)[UNKNOWN_XML_SCAN_CONTEXT];
    if (context) return context.links;
  }

  const cached = (lexer as { [UNKNOWN_XML_LEXER_LINKS]?: Links })[
    UNKNOWN_XML_LEXER_LINKS
  ];
  if (cached) return cached;
  if (src === undefined) {
    if (!Array.isArray(tokens)) return undefined;
    return (tokens as Token[] & { links?: Links }).links;
  }

  const links = collectReferenceLinks(src);
  Object.defineProperty(lexer, UNKNOWN_XML_LEXER_LINKS, {
    configurable: true,
    value: links,
  });
  return links;
}

function tagInputEnd(src: string, offset: number, end: number): number {
  if (src.startsWith("<!--", offset)) {
    const commentEnd = src.indexOf("-->", offset + 4);
    return commentEnd < 0 ? end : commentEnd + 3;
  }
  if (src.startsWith("<?", offset)) {
    const processingEnd = src.indexOf("?>", offset + 2);
    return processingEnd < 0 ? end : processingEnd + 2;
  }
  if (src.startsWith("<![CDATA[", offset)) {
    const cdataEnd = src.indexOf("]]>", offset + 9);
    return cdataEnd < 0 ? end : cdataEnd + 3;
  }

  const next = src[offset + 1];
  if (next === "!" && !/[A-Za-z]/.test(src[offset + 2] ?? "")) {
    return offset + 1;
  }
  const nameOffset = next === "/" ? offset + 2 : offset + 1;
  const nameStart = src[nameOffset] ?? "";
  const namedTag =
    (next === "/" || (next !== "!" && next !== "?")) &&
    nameOffset < end &&
    /[A-Za-z]/.test(nameStart);
  if (!namedTag && next !== "!") return offset + 1;

  let nameEnd = nameOffset;
  if (namedTag) {
    while (nameEnd < end && /[A-Za-z0-9:_-]/.test(src[nameEnd] ?? "")) {
      nameEnd += 1;
    }
    if (src[nameEnd] === "<") return offset + 1;
  }

  let quote: string | undefined;
  for (let cursor = offset + 1; cursor < end; cursor += 1) {
    const char = src[cursor];
    if (quote) {
      if (char === quote) quote = undefined;
    } else if (char === '"' || char === "'") {
      quote = char;
    } else if (char === ">") {
      return cursor + 1;
    } else if (namedTag && char === "<") {
      return offset + 1;
    }
  }
  return offset + 1;
}

function rawHtmlEnd(src: string, start: number, raw: string): number | undefined {
  const opening = /^<(pre|script|style|textarea)\b/i.exec(raw);
  if (!opening || raw.startsWith("</") || isSelfClosingTag(raw)) return undefined;
  const closing = new RegExp(`</${opening[1]}\\s*>`, "ig");
  closing.lastIndex = start + raw.length;
  const match = closing.exec(src);
  return match ? match.index + match[0].length : src.length;
}

function nextScannerToken(
  src: string,
  offset: number,
  end: number,
  scanner: MarkdownScanner,
): ScannerToken | undefined {
  if (offset >= end) return undefined;
  if (scanner.ignoredFenceRange) {
    if (offset >= scanner.ignoredFenceRange.end) {
      scanner.ignoredFenceRange = undefined;
    } else if (src[offset] === "`") {
      let ignoredEnd = offset + 1;
      while (ignoredEnd < scanner.ignoredFenceRange.end && src[ignoredEnd] === "`") {
        ignoredEnd += 1;
      }
      return { kind: "ignored", start: offset, end: ignoredEnd };
    }
  }
  const atLineStart = offset === 0 || src[offset - 1] === "\n";

  if (atLineStart) {
    if (
      src[offset] === " " ||
      src[offset] === "\t" ||
      src[offset] === "`" ||
      src[offset] === "~"
    ) {
      const rest = src.slice(offset, end);
      const blockCode = scanner.tokenizer.code(rest) ?? scanner.tokenizer.fences(rest);
      if (blockCode) {
        return { kind: "protected", start: offset, end: offset + blockCode.raw.length };
      }
      const fenceLine = /^ {0,3}(`{3,}|~{3,})([^\r\n]*)(?:\r?\n|$)/.exec(rest);
      if (fenceLine?.[1]?.startsWith("`") && fenceLine[2]?.includes("`")) {
        const lineEnd = offset + fenceLine[0].replace(/\r?\n$/, "").length;
        scanner.ignoredFenceRange = { start: offset, end: lineEnd };
      }
    }
  }

  if (src[offset] === "\\") {
    const rest = src.slice(offset, Math.min(end, offset + 2));
    const escaped = scanner.tokenizer.escape(rest);
    if (escaped) {
      return { kind: "protected", start: offset, end: offset + escaped.raw.length };
    }
  }

  if (src[offset] === "<") {
    const inputEnd = tagInputEnd(src, offset, end);
    const rest = src.slice(offset, inputEnd);
    if (
      src.startsWith("<!--", offset) ||
      src.startsWith("<?", offset) ||
      src.startsWith("<![CDATA[", offset) ||
      (src[offset + 1] === "!" && /[A-Za-z]/.test(src[offset + 2] ?? ""))
    ) {
      return { kind: "protected", start: offset, end: inputEnd };
    }
    const tag = scanner.tokenizer.tag(rest);
    if (tag) {
      const raw = tag.raw;
      if (raw.startsWith("<!--") || /^<!|^<\?/i.test(raw)) {
        return { kind: "protected", start: offset, end: inputEnd };
      }
      const rawEnd = rawHtmlEnd(src, offset, raw);
      if (rawEnd !== undefined) {
        return { kind: "protected", start: offset, end: rawEnd };
      }
      return { kind: "tag", start: offset, end: offset + raw.length, raw };
    }
  }

  if (src[offset] === "[" || (src[offset] === "!" && src[offset + 1] === "[")) {
    const rest = src.slice(offset, end);
    const link = scanner.tokenizer.link(rest);
    if (link) {
      return { kind: "protected", start: offset, end: offset + link.raw.length };
    }
    const reference = scanner.tokenizer.reflink(rest, scanner.links);
    if (reference && reference.type !== "text") {
      return { kind: "protected", start: offset, end: offset + reference.raw.length };
    }
  }

  if (src[offset] === "`") {
    const rest = src.slice(offset, end);
    const code = scanner.tokenizer.codespan(rest);
    if (code) {
      return { kind: "protected", start: offset, end: offset + code.raw.length };
    }
  }

  if (src[offset] === "*" || src[offset] === "_") {
    const rest = src.slice(offset, end);
    const emphasis = scanner.tokenizer.emStrong(
      rest,
      rest,
      offset > 0 ? src[offset - 1] : "",
    );
    if (emphasis) {
      return { kind: "protected", start: offset, end: offset + emphasis.raw.length };
    }
  }

  if (src[offset] === "~") {
    const rest = src.slice(offset, end);
    const strikethrough = scanner.tokenizer.del(
      rest,
      rest,
      offset > 0 ? src[offset - 1] : "",
    );
    if (strikethrough) {
      return {
        kind: "protected",
        start: offset,
        end: offset + strikethrough.raw.length,
      };
    }
  }

  if (src[offset] === "<") {
    const rest = src.slice(offset, tagInputEnd(src, offset, end));
    const autolink = scanner.tokenizer.autolink(rest);
    if (autolink && isProtectedAutolink(autolink.raw)) {
      return { kind: "protected", start: offset, end: offset + autolink.raw.length };
    }
  }

  return undefined;
}

function scanMarkdownTokens(
  src: string,
  start: number,
  end: number,
  links: Links,
  visit: (token: ScannerToken) => boolean,
): void {
  const scanner = createMarkdownScanner(links);
  let offset = start;
  while (offset < end) {
    const token = nextScannerToken(src, offset, end, scanner);
    if (token) {
      if (!visit(token)) return;
      offset = Math.max(token.end, offset + 1);
      continue;
    }
    offset += 1;
  }
}

function matchUnknownXmlBlockAt(
  src: string,
  offset: number,
  end = src.length,
  allowIncomplete = false,
  links: Links = {},
  recordResult?: (start: number, end: number | undefined) => void,
): number | undefined {
  if (offset < 0 || offset >= src.length || (offset > 0 && src[offset - 1] !== "\n")) {
    recordResult?.(offset, undefined);
    return undefined;
  }

  const opening = tagAtLineStart(src, offset, end);
  if (!opening) {
    recordResult?.(offset, undefined);
    return undefined;
  }

  const openingText = opening[0];
  const openingTagText = openingText.trimStart();
  const openingName = opening[1]?.toLowerCase();
  if (
    !openingName ||
    openingTagText.startsWith("</") ||
    isSelfClosingTag(openingTagText) ||
    isPreservedHtmlTag(openingName)
  ) {
    recordResult?.(offset, undefined);
    return undefined;
  }

  const stack = [{ name: openingName, start: offset }];
  const openHtmlTags: string[] = [];
  let result: number | undefined;
  let failed = false;
  scanMarkdownTokens(src, offset + openingText.length, end, links, (token) => {
    if (token.kind === "protected" || token.kind === "ignored") return true;
    const tag = new RegExp(`^${XML_TAG_ESCAPE_RE.source}`).exec(token.raw);
    const name = tag?.[1]?.toLowerCase();
    if (!name) return true;

    const closing = token.raw.startsWith("</");
    const selfClosing = isSelfClosingTag(token.raw);
    if (isPreservedHtmlTag(name)) {
      if (closing) {
        if (openHtmlTags.at(-1) === name) openHtmlTags.pop();
      } else if (!selfClosing && !VOID_HTML_TAGS.has(name)) {
        openHtmlTags.push(name);
      }
      return true;
    }

    if (openHtmlTags.length > 0 || selfClosing || isProtectedAutolink(token.raw)) return true;
    if (closing) {
      if (stack.at(-1)?.name !== name) {
        // Namespaced openers such as <ns:item> scan as URI autolinks, so their
        // closers have no stack entry to pair with.
        if (name.includes(":")) return true;
        failed = true;
        return false;
      }
      stack.pop();
      if (stack.length === 0) {
        result = token.end;
        recordResult?.(offset, result);
        return false;
      }
      return true;
    }

    stack.push({ name, start: token.start });
    return true;
  });

  if (failed) {
    for (const entry of stack) recordResult?.(entry.start, undefined);
    return undefined;
  }
  if (result === undefined) {
    for (const entry of stack) recordResult?.(entry.start, undefined);
  }
  return result ?? (allowIncomplete ? end : undefined);
}

function updateKnownHtmlTags(stack: string[], tagText: string, name: string): void {
  if (!isPreservedHtmlTag(name)) return;
  if (tagText.startsWith("</")) {
    if (stack.at(-1) === name) stack.pop();
  } else if (!isSelfClosingTag(tagText) && !VOID_HTML_TAGS.has(name)) {
    stack.push(name);
  }
}

function findUnknownXmlCandidate(
  src: string,
  links?: Links,
  scan?: { context: UnknownXmlScanContext; offset: number },
): number | undefined {
  if (src.indexOf("<") < 0) return undefined;

  const referenceLinks = links ?? collectReferenceLinks(src);
  const base = scan?.offset ?? 0;
  const candidateResults = scan?.context.candidateResults;
  const cached = candidateResults?.get(base);
  if (candidateResults?.has(base)) {
    return cached === undefined ? undefined : cached - base;
  }

  const blankLine = /(?:^|\n)[ \t]*(?:\n|$)/.exec(src);
  const windowEnd = blankLine?.index ?? src.length;
  const openHtmlTags: string[] = [];
  const scanner = createMarkdownScanner(referenceLinks);
  let cursor = 0;
  let atLineStart = true;

  while (cursor < windowEnd) {
    if (atLineStart) {
      const lineTag = tagAtLineStart(src, cursor, windowEnd);
      if (lineTag) {
        const tagBody = lineTag[0].trimStart();
        const name = lineTag[1]?.toLowerCase();
        if (
          name &&
          openHtmlTags.length === 0 &&
          !tagBody.startsWith("</") &&
          !isSelfClosingTag(tagBody) &&
          !isPreservedHtmlTag(name) &&
          !isProtectedAutolink(tagBody)
        ) {
          const absoluteStart = base + cursor;
          const cachedMatch = scan?.context.results.get(absoluteStart);
          if (scan?.context.results.has(absoluteStart)) {
            if (cachedMatch === undefined) return undefined;
            candidateResults?.set(base, absoluteStart);
            return cursor;
          }

          const matched = matchUnknownXmlBlockAt(
            src,
            cursor,
            src.length,
            false,
            scanner.links,
            scan
              ? (localStart, localEnd) => {
                  scan.context.results.set(
                    base + localStart,
                    localEnd === undefined ? undefined : base + localEnd,
                  );
                }
              : undefined,
          );
          if (matched === undefined) return undefined;
          candidateResults?.set(base, absoluteStart);
          return cursor;
        }
      }
    }

    const token = nextScannerToken(src, cursor, windowEnd, scanner);
    if (token) {
      if (token.kind === "tag") {
        const tag = new RegExp("^" + XML_TAG_ESCAPE_RE.source).exec(token.raw);
        const name = tag?.[1]?.toLowerCase();
        if (name) updateKnownHtmlTags(openHtmlTags, token.raw, name);
      }
      cursor = Math.max(token.end, cursor + 1);
      atLineStart = src[cursor - 1] === "\n";
      continue;
    }

    const char = src[cursor];
    if (char === "\n") {
      atLineStart = true;
    } else if (!(atLineStart && (char === " " || char === "\t"))) {
      atLineStart = false;
    }
    cursor += 1;
  }

  candidateResults?.set(base, undefined);
  return undefined;
}

function unknownXmlBlockExtension(): TokenizerAndRendererExtension {
  return {
    name: "unknownXmlBlock",
    level: "block",
    tokenizer(src, tokens) {
      const scan = getUnknownXmlScanContext(tokens, src, getLexerLinks(this, src));
      const base = scan?.offset ?? 0;
      const cachedEnd = scan?.context.results.get(base);
      if (scan?.context.results.has(base)) {
        if (cachedEnd === undefined) return undefined;
        const raw = src.slice(0, cachedEnd - base);
        return { type: "unknownXmlBlock", raw, text: raw };
      }

      const end = matchUnknownXmlBlockAt(
        src,
        0,
        src.length,
        false,
        scan?.context.links,
        scan
          ? (localStart, localEnd) => {
              scan.context.results.set(
                base + localStart,
                localEnd === undefined ? undefined : base + localEnd,
              );
            }
          : undefined,
      );
      if (end === undefined) return undefined;
      const raw = src.slice(0, end);
      return { type: "unknownXmlBlock", raw, text: raw };
    },
    renderer(token) {
      const text = escapeHTML(token.text.replace(/\n$/, "") + "\n");
      return `<pre class="unknown-xml-block"><code>${text}</code></pre>\n`;
    },
  };
}

function unknownXmlParagraphBoundary() {
  return {
    paragraph(src: string) {
      const scan = getUnknownXmlScanContext(
        getLexerTokens(this),
        src,
        getLexerLinks(this, src),
      );
      const candidate = findUnknownXmlCandidate(src, scan?.context.links, scan);
      if (candidate === undefined || candidate === 0) return false;
      return Tokenizer.prototype.paragraph.call(this, src.slice(0, candidate)) ?? false;
    },
  };
}

function unknownXmlHtmlBoundary() {
  return {
    html(src: string) {
      if (/^<(?:!|\?|script\b|pre\b|style\b|textarea\b)/i.test(src)) return false;
      const scan = getUnknownXmlScanContext(
        getLexerTokens(this),
        src,
        getLexerLinks(this, src),
      );
      const candidate = findUnknownXmlCandidate(src, scan?.context.links, scan);
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
      ? { tokenizer: { ...unknownXmlHtmlBoundary(), ...unknownXmlParagraphBoundary() } }
      : {}),
  });

  return instance;
}

const parser = createParser(false);
const preformattedParser = createParser(true);

type RenderCacheEntry = [string | undefined, string | undefined];

const cache = new LRUCache<string, RenderCacheEntry>(6000);

const ASSET_PLACEHOLDER_PREFIX = "/__agentsview_asset__/";

function resolveAssetURLs(text: string): string {
  return text.replace(/asset:\/\/([^\s)]+)/g, (_match, reference: string) =>
    `${ASSET_PLACEHOLDER_PREFIX}${encodeURIComponent(reference)}`,
  );
}

function getAssetReference(src: string | null): string | undefined {
  if (!src?.startsWith(ASSET_PLACEHOLDER_PREFIX)) return undefined;
  try {
    return decodeURIComponent(src.slice(ASSET_PLACEHOLDER_PREFIX.length));
  } catch {
    return undefined;
  }
}

function getAssetURL(reference: string): string {
  return `${getBase().replace(/\/$/, "")}/assets/${encodeURIComponent(reference)}`;
}

export function loadAssetImages(node: HTMLElement, _content = "") {
  let destroyed = false;
  const blobURLs = new Set<string>();

  async function load(): Promise<void> {
    const images = [...node.querySelectorAll<HTMLImageElement>("img")];
    await Promise.all(
      images.map(async (image) => {
        const reference = getAssetReference(image.getAttribute("src"));
        if (!reference || image.dataset.agentsviewAsset === reference) return;
        image.dataset.agentsviewAsset = reference;

        try {
          const response = await fetch(getAssetURL(reference), authHeaders());
          if (!response.ok) throw new Error(`asset request failed: ${response.status}`);
          const blobURL = URL.createObjectURL(await response.blob());
          if (destroyed || !node.contains(image)) {
            URL.revokeObjectURL(blobURL);
            return;
          }
          image.src = blobURL;
          blobURLs.add(blobURL);
        } catch {
          delete image.dataset.agentsviewAsset;
        }
      }),
    );
  }

  void load();

  return {
    update() {
      void load();
    },
    destroy() {
      destroyed = true;
      for (const blobURL of blobURLs) URL.revokeObjectURL(blobURL);
      blobURLs.clear();
    },
  };
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
    /^[A-Za-z][A-Za-z0-9+.-]{1,31}:/.test(inner) ||
    /^mailto:/i.test(inner) ||
    /^[^\s<>@]+@[^\s<>]+$/.test(inner)
  );
}

function isLegacyProtectedAutolink(raw: string): boolean {
  const inner = raw.slice(1, -1);
  return (
    /^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(inner) ||
    /^mailto:/i.test(inner) ||
    /^[^\s<>@]+@[^\s<>]+$/.test(inner)
  );
}

function shouldEscapeCustomXmlLiteral(raw: string | undefined): boolean {
  if (!raw || isLegacyProtectedAutolink(raw)) {
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
