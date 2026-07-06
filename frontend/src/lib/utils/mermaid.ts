// kit-ui-check-ignore: Mermaid returns raw SVG, not markdown HTML; this path keeps a strict SVG-only DOMPurify profile before {@html} injection.
import DOMPurify from "dompurify";

type MermaidModule = typeof import("mermaid");

export type MermaidRenderErrorKind =
  | "load"
  | "initialize"
  | "render"
  | "sanitize";

export interface MermaidRenderError {
  kind: MermaidRenderErrorKind;
  message: string;
}

export type MermaidRenderResult =
  | { ok: true; svg: string }
  | { ok: false; error: MermaidRenderError };

let mermaidModulePromise: Promise<MermaidModule> | null = null;
let mermaidApiPromise: Promise<MermaidModule["default"]> | null = null;
let mermaidRenderId = 0;

function toError(kind: MermaidRenderErrorKind, error: unknown): MermaidRenderError {
  return {
    kind,
    message: error instanceof Error ? error.message : String(error),
  };
}

async function loadMermaid(): Promise<MermaidModule> {
  if (!mermaidModulePromise) {
    mermaidModulePromise = import("mermaid");
  }
  return mermaidModulePromise;
}

async function getMermaidApi(): Promise<MermaidModule["default"]> {
  if (!mermaidApiPromise) {
    mermaidApiPromise = (async () => {
      const module = await loadMermaid();
      const api = module.default;
      api.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        htmlLabels: false,
      });
      return api;
    })();
  }
  return mermaidApiPromise;
}

function sanitizeSvg(svg: string): string | null {
  const sanitized = DOMPurify.sanitize(svg, {
    USE_PROFILES: { svg: true, svgFilters: true },
  });
  if (typeof sanitized !== "string") return null;
  const trimmed = sanitized.trim();
  return trimmed.includes("<svg") ? trimmed : null;
}

export async function renderMermaid(
  source: string,
): Promise<MermaidRenderResult> {
  try {
    const api = await getMermaidApi();
    const id = `agentsview-mermaid-${++mermaidRenderId}`;
    const { svg } = await api.render(id, source);
    const sanitized = sanitizeSvg(svg);
    if (!sanitized) {
      return {
        ok: false,
        error: {
          kind: "sanitize",
          message: "Mermaid returned an empty or invalid SVG.",
        },
      };
    }
    return { ok: true, svg: sanitized };
  } catch (error) {
    const kind =
      error instanceof Error &&
      /import|module|load/i.test(error.message)
        ? "load"
        : error instanceof Error &&
          /initialize/i.test(error.message)
          ? "initialize"
          : "render";
    return { ok: false, error: toError(kind, error) };
  }
}
