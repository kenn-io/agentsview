// worker.js injects claude_fetch.js first. Grok reads its REST API with the
// page's cookies; the allowed request shapes are plain GETs.
async function grokFetch(url) {
  return globalThis.claudeFetch(url, { Accept: "application/json" });
}
