import { Marked, type TokenizerExtension } from "marked";
import DOMPurify from "dompurify";
import { LRUCache } from "./cache.js";

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

export function renderMarkdown(text: string): string {
  if (!text) return "";

  const cached = cache.get(text);
  if (cached !== undefined) return cached;

  const resolved = resolveAssetURLs(text);

  // Trim trailing whitespace — with breaks:true, trailing
  // newlines become <br> tags that add invisible height.
  const html = parser.parse(resolved.trimEnd()) as string;
  const safe = DOMPurify.sanitize(html);

  cache.set(text, safe);
  return safe;
}
