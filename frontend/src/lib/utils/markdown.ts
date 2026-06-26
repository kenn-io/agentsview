import { Marked, type TokenizerExtension } from "marked";
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
const BASH_WRAPPER_BLOCK_RE =
  /<bash-(input|stdout|stderr)>[\s\S]*?<\/bash-\1>/g;
const FENCED_CODE_BLOCK_RE =
  /(^|\n)(?: {0,3})(`{3,}|~{3,})[^\n]*\n[\s\S]*?\n(?: {0,3})\2[ \t]*(?=\n|$)/g;
const INLINE_CODE_SPAN_RE = /(`+)([\s\S]*?)\1/g;
const AUTOLINK_RE =
  /<(?:[A-Za-z][A-Za-z0-9+.-]{1,31}:[^<>\s]+|[^\s<>@]+@[^\s<>]+)>/g;

type ProtectedRange = {
  start: number;
  end: number;
};

/** Build a marked tokenizer extension that consumes a Claude Code
 *  shell-shortcut wrapper tag and emits a `code` token directly.
 *  Because this runs at the lexer level, occurrences of the tag
 *  inside a fenced code block are never reached — marked has
 *  already consumed those characters as a `code` token. */
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
        // Drop empty wrappers entirely (common for stdout/stderr).
        return { type: "space", raw: m[0] };
      }
      // Preserve the captured whitespace verbatim — code blocks
      // are expected to render shell output exactly, including
      // indentation and trailing blank lines.
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

function escapeCustomXmlTagsSegment(text: string): string {
  return text.replace(XML_TAG_ESCAPE_RE, (tag, rawName: string) => {
    const name = rawName.toLowerCase();
    if (
      KNOWN_HTML_TAGS.has(name) ||
      name === "bash-input" ||
      name === "bash-stdout" ||
      name === "bash-stderr"
    ) {
      return tag;
    }
    return tag.replace(/</g, "&lt;").replace(/>/g, "&gt;");
  });
}

function collectProtectedRanges(
  text: string,
  patterns: RegExp[],
): ProtectedRange[] {
  const matches: ProtectedRange[] = [];

  for (const pattern of patterns) {
    const re = new RegExp(pattern.source, pattern.flags);
    let match: RegExpExecArray | null;
    while ((match = re.exec(text)) !== null) {
      const raw = match[0];
      if (!raw) {
        re.lastIndex += 1;
        continue;
      }
      matches.push({
        start: match.index,
        end: match.index + raw.length,
      });
    }
  }

  matches.sort((a, b) => a.start - b.start || b.end - a.end);

  const merged: ProtectedRange[] = [];
  for (const match of matches) {
    const last = merged[merged.length - 1];
    if (!last || match.start >= last.end) {
      merged.push(match);
      continue;
    }
    if (match.end > last.end) {
      last.end = match.end;
    }
  }

  return merged;
}

function escapeCustomXmlTags(text: string): string {
  const protectedRanges = collectProtectedRanges(text, [
    BASH_WRAPPER_BLOCK_RE,
    FENCED_CODE_BLOCK_RE,
    INLINE_CODE_SPAN_RE,
    AUTOLINK_RE,
  ]);

  if (protectedRanges.length === 0) {
    return escapeCustomXmlTagsSegment(text);
  }

  let cursor = 0;
  let result = "";

  for (const range of protectedRanges) {
    if (cursor < range.start) {
      result += escapeCustomXmlTagsSegment(
        text.slice(cursor, range.start),
      );
    }
    result += text.slice(range.start, range.end);
    cursor = range.end;
  }

  if (cursor < text.length) {
    result += escapeCustomXmlTagsSegment(text.slice(cursor));
  }

  return result;
}

export function renderMarkdown(text: string): string {
  if (!text) return "";

  const cached = cache.get(text);
  if (cached !== undefined) return cached;

  const resolved = escapeCustomXmlTags(resolveAssetURLs(text));

  // Trim trailing whitespace — with breaks:true, trailing
  // newlines become <br> tags that add invisible height.
  const html = parser.parse(resolved.trimEnd()) as string;
  const safe = DOMPurify.sanitize(html);

  cache.set(text, safe);
  return safe;
}
