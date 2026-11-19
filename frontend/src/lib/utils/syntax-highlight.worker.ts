import { highlightInline, type HighlightOptions } from "./syntax-highlight.js";

self.onmessage = async ({
  data,
}: MessageEvent<{ id: number; code: string; lang: string; options?: HighlightOptions }>) => {
  const html = await highlightInline(data.code, data.lang, data.options ?? {});
  self.postMessage({ id: data.id, html });
};
