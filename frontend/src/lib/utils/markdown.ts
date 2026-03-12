import { Marked } from "marked";
import DOMPurify from "dompurify";
import { LRUCache } from "./cache.js";

/**
 * Strip markdown syntax from text, returning a plain-text approximation
 * suitable for search matching. Keeps human-visible content (link text,
 * inline code content, heading text) while removing syntax characters
 * and URLs that would not appear in the rendered DOM.
 */
export function stripMarkdown(text: string): string {
  return (
    text
      // Fenced code blocks — keep content
      .replace(/```[^\n]*\n([\s\S]*?)```/g, "$1")
      // Inline code — keep content
      .replace(/`([^`\n]+)`/g, "$1")
      // Images — keep alt text
      .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
      // Links — keep link text, drop URL
      .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
      // Bold / italic (up to ***triple***)
      .replace(/\*{1,3}([^*\n]+)\*{1,3}/g, "$1")
      .replace(/_{1,3}([^_\n]+)_{1,3}/g, "$1")
      // ATX headings
      .replace(/^#{1,6}\s+/gm, "")
      // Blockquotes
      .replace(/^>\s*/gm, "")
      // Setext heading underlines
      .replace(/^[=\-]{3,}\s*$/gm, "")
      // HTML tags
      .replace(/<[^>]+>/g, "")
      // Remaining stray syntax characters
      .replace(/[*_`~]/g, "")
  );
}

const parser = new Marked({
  gfm: true,
  breaks: true,
});

const cache = new LRUCache<string, string>(6000);

export function renderMarkdown(text: string): string {
  if (!text) return "";

  const cached = cache.get(text);
  if (cached !== undefined) return cached;

  // Trim trailing whitespace — with breaks:true, trailing
  // newlines become <br> tags that add invisible height.
  const html = parser.parse(text.trimEnd()) as string;
  const safe = DOMPurify.sanitize(html);

  cache.set(text, safe);
  return safe;
}
