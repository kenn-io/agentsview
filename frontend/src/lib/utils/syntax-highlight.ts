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
const GRAMMAR_LOADERS: Record<string, () => Promise<{ default: LanguageRegistration[] }>> = {
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
    // Grammars do not depend on the core modules, so every chunk loads at once.
    const [
      { createHighlighterCore },
      { createJavaScriptRegexEngine },
      theme,
      lightTheme,
      langModules,
    ] = await Promise.all([
      import("shiki/core"),
      import("shiki/engine/javascript"),
      import("shiki/themes/catppuccin-mocha.mjs"),
      import("shiki/themes/github-light.mjs"),
      Promise.all(Object.values(GRAMMAR_LOADERS).map((load) => load())),
    ]);

    const hl = await createHighlighterCore({
      themes: [theme.default ?? theme, lightTheme.default ?? lightTheme],
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

export interface HighlightOptions {
  /**
   * Also emit github-light token colors as `--shiki-light` (and
   * `--shiki-light-font-style`) on each token, for a container that is
   * light in the light theme. The inline color stays catppuccin-mocha, but
   * its italics move to `--shiki-dark-font-style`, so the container must
   * apply the font-style variable for the active theme.
   */
  lightTheme?: boolean;
}

export async function highlightInline(
  code: string,
  lang: string,
  options: HighlightOptions = {},
): Promise<string | null> {
  const lineEndings = code.match(/\r\n|\r|\n/g) ?? [];

  try {
    const hl = await getHighlighter();

    const source = code.replace(/\r\n?/g, "\n");
    const html = options.lightTheme
      ? hl.codeToHtml(source, {
          lang,
          themes: { dark: "catppuccin-mocha", light: "github-light" },
          defaultColor: "dark",
          structure: "inline",
        })
      : hl.codeToHtml(source, {
          lang,
          theme: "catppuccin-mocha",
          structure: "inline",
        });

    // Shiki's inline layout emits <br>, which is absent from textContent.
    // Restore source separators in our preformatted code containers. Literal
    // <br> in source is already escaped by Shiki and cannot match this markup.
    // Encode CR so assigning innerHTML does not normalize CRLF into LF.
    let line = 0;
    return html.replaceAll("<br>", () => (lineEndings[line++] ?? "\n").replaceAll("\r", "&#13;"));
  } catch {
    return null;
  }
}

let worker: Worker | null = null;
let workerFailed = false;
let nextId = 0;
const pending = new Map<
  number,
  {
    code: string;
    lang: string;
    options: HighlightOptions;
    resolve: (html: string | null | PromiseLike<string | null>) => void;
  }
>();

function failWorker() {
  workerFailed = true;
  worker?.terminate();
  worker = null;
  for (const { code, lang, options, resolve } of pending.values()) {
    resolve(highlightInline(code, lang, options));
  }
  pending.clear();
}

/**
 * Highlight `code` for `lang` using Shiki (catppuccin-mocha, structure: "inline").
 * Shiki runs in a worker, with inline highlighting if the worker fails.
 * Returns null for unknown languages, over-threshold content, or any error.
 * Callers insert the result via `{@html}` — Shiki escapes the code text itself.
 * Line endings remain text nodes so copying and search retain source offsets.
 */
export async function highlightToHtml(
  code: string,
  lang: string,
  options: HighlightOptions = {},
): Promise<string | null> {
  const resolved = resolveLanguage(lang);
  if (resolved === null || code.length > HIGHLIGHT_MAX_BYTES) return null;
  // Reject large blocks before starting either highlighting runtime.
  if ((code.match(/\r\n|\r|\n/g)?.length ?? 0) + 1 > HIGHLIGHT_MAX_LINES) return null;
  if (workerFailed || typeof Worker === "undefined")
    return highlightInline(code, resolved, options);

  if (worker === null) {
    try {
      worker = new Worker(new URL("./syntax-highlight.worker.ts", import.meta.url), {
        type: "module",
      });
      worker.onmessage = ({ data }: MessageEvent<{ id: number; html: string | null }>) => {
        pending.get(data.id)?.resolve(data.html);
        pending.delete(data.id);
      };
      worker.onerror = failWorker;
      worker.onmessageerror = failWorker;
    } catch {
      failWorker();
      return highlightInline(code, resolved, options);
    }
  }

  return new Promise((resolve) => {
    const id = nextId++;
    pending.set(id, { code, lang: resolved, options, resolve });
    worker!.postMessage({ id, code, lang: resolved, options });
  });
}
