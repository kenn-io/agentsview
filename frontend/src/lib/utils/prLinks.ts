import type { ParserPRLink } from "../api/generated/index.js";

/** Returns the URL when it is an absolute http(s) URL, otherwise null.
 *  Session data comes from transcripts, so other schemes (javascript:,
 *  data:, file:) must never become a link target. */
export function safeExternalHref(url: string | null | undefined): string | null {
  if (!url) return null;
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return null;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    return null;
  }
  return parsed.href;
}

/** Short display text for a pull request link, like `owner/repo#123`. */
export function prLinkLabel(link: Pick<ParserPRLink, "repository" | "number" | "url">): string {
  const repository = link.repository.trim();
  if (repository && link.number > 0) return `${repository}#${link.number}`;
  if (repository) return repository;
  return link.url;
}

export interface DisplayPRLink {
  href: string;
  label: string;
}

/** Pull request links that are safe to render, deduplicated by URL in the
 *  order the session reports them. */
export function displayPRLinks(links: readonly ParserPRLink[] | null | undefined): DisplayPRLink[] {
  const out: DisplayPRLink[] = [];
  const seen = new Set<string>();
  for (const link of links ?? []) {
    const href = safeExternalHref(link.url);
    if (!href || seen.has(href)) continue;
    seen.add(href);
    out.push({ href, label: prLinkLabel(link) });
  }
  return out;
}
