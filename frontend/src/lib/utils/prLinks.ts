import type { DbPRLink } from "../api/generated/index.js";

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
export function prLinkLabel(link: Pick<DbPRLink, "repository" | "number" | "url">): string {
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
export function displayPRLinks(links: readonly DbPRLink[] | null | undefined): DisplayPRLink[] {
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

const PR_NUMBER_RE = /^\d+$/;

/** Reports whether a pull request filter has a form the server accepts:
 *  `owner/repo`, `owner/repo#123`, or an http(s) URL. The server makes
 *  the final call on URLs; this check only catches values it would
 *  reject outright. An empty value is valid and clears the filter. */
export function isValidPRFilter(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return true;
  if (trimmed.includes("://")) return safeExternalHref(trimmed) !== null;
  const hashIndex = trimmed.indexOf("#");
  const repo = (hashIndex === -1 ? trimmed : trimmed.slice(0, hashIndex))
    .trim()
    .replace(/^\/+|\/+$/g, "");
  if (!repo.includes("/") || /\s/.test(repo)) return false;
  if (hashIndex === -1) return true;
  const num = trimmed.slice(hashIndex + 1).trim();
  return PR_NUMBER_RE.test(num) && Number(num) > 0;
}
