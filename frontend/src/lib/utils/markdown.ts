import { Marked } from "marked";
import DOMPurify from "dompurify";
import { LRUCache } from "./cache.js";

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
