import { highlightInline } from "./syntax-highlight.js";

self.onmessage = async ({ data }: MessageEvent<{ id: number; code: string; lang: string }>) => {
  const html = await highlightInline(data.code, data.lang);
  self.postMessage({ id: data.id, html });
};
