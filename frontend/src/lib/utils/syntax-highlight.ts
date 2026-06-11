import type { HighlighterCore, LanguageRegistration } from "shiki/core";

/** Skip highlighting when code exceeds this byte count. */
export const HIGHLIGHT_MAX_BYTES = 50_000;

/** Skip highlighting when code exceeds this line count. */
export const HIGHLIGHT_MAX_LINES = 800;

// Common short names → canonical preloaded language ids.
const LANG_ALIASES: Record<string, string> = {
  js: "javascript",
  ts: "typescript",
  py: "python",
  sh: "bash",
  zsh: "bash",
  shell: "bash",
  yml: "yaml",
  golang: "go",
  md: "markdown",
  htm: "html",
};

// Each entry must be a literal import() string so Vite can statically analyze
// and emit a separate lazy chunk per grammar. Template-literal imports are not
// analyzable by Vite and would be omitted from the production dist/.
const GRAMMAR_LOADERS: Record<
  string,
  () => Promise<{ default: LanguageRegistration[] }>
> = {
  javascript: () => import("shiki/langs/javascript.mjs"),
  typescript: () => import("shiki/langs/typescript.mjs"),
  python: () => import("shiki/langs/python.mjs"),
  go: () => import("shiki/langs/go.mjs"),
  bash: () => import("shiki/langs/bash.mjs"),
  json: () => import("shiki/langs/json.mjs"),
  yaml: () => import("shiki/langs/yaml.mjs"),
  markdown: () => import("shiki/langs/markdown.mjs"),
  html: () => import("shiki/langs/html.mjs"),
  css: () => import("shiki/langs/css.mjs"),
  rust: () => import("shiki/langs/rust.mjs"),
  sql: () => import("shiki/langs/sql.mjs"),
};

const PRELOADED_LANGS = Object.keys(GRAMMAR_LOADERS);

let highlighterPromise: Promise<HighlighterCore> | null = null;

function getHighlighter(): Promise<HighlighterCore> {
  if (highlighterPromise !== null) return highlighterPromise;

  highlighterPromise = (async () => {
    const [{ createHighlighterCore }, { createJavaScriptRegexEngine }, theme] =
      await Promise.all([
        import("shiki/core"),
        import("shiki/engine/javascript"),
        import("shiki/themes/catppuccin-mocha.mjs"),
      ]);

    const langModules = await Promise.all(
      Object.values(GRAMMAR_LOADERS).map((load) => load()),
    );

    const hl = await createHighlighterCore({
      themes: [theme.default ?? theme],
      langs: langModules.map((m) => m.default),
      engine: createJavaScriptRegexEngine(),
    });

    return hl;
  })().catch((err) => {
    // Reset so a future call may retry after a transient error.
    highlighterPromise = null;
    throw err;
  });

  return highlighterPromise;
}

function resolveLanguage(lang: string): string | null {
  const trimmed = lang.trim().toLowerCase();
  if (!trimmed) return null;
  const resolved = LANG_ALIASES[trimmed] ?? trimmed;
  return PRELOADED_LANGS.includes(resolved) ? resolved : null;
}

/**
 * Highlight `code` for `lang` using Shiki (catppuccin-mocha, structure: "inline").
 * Returns null for unknown languages, over-threshold content, or any error.
 * Callers insert the result via `{@html}` — Shiki escapes the code text itself.
 */
export async function highlightToHtml(
  code: string,
  lang: string,
): Promise<string | null> {
  const resolved = resolveLanguage(lang);
  if (resolved === null) return null;

  // Guard against large blocks that would stall the main thread.
  if (code.length > HIGHLIGHT_MAX_BYTES) return null;
  if (code.split("\n").length > HIGHLIGHT_MAX_LINES) return null;

  try {
    const hl = await getHighlighter();

    return hl.codeToHtml(code, {
      lang: resolved,
      theme: "catppuccin-mocha",
      structure: "inline",
    });
  } catch {
    return null;
  }
}
