// worker.js injects this file, then calls pageFetch, or a platform script
// that wraps it, from a second script. headers adds request headers.
globalThis.pageFetch = async function pageFetch(url, headers) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
    const response = await fetch(url, {
      method: "GET",
      credentials: "include",
      redirect: "error",
      headers,
      signal: controller.signal,
    });
    const limit = 32 * 1024 * 1024; // Matches importer.BrowserResponseLimit.
    const reader = response.body?.getReader();
    const decoder = new TextDecoder();
    let size = 0;
    let body = "";
    if (reader) {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.byteLength;
        if (size > limit) {
          controller.abort();
          try {
            await reader.cancel();
          } catch {}
          return { status: 413, body: "" };
        }
        body += decoder.decode(value, { stream: true });
      }
      body += decoder.decode();
    }
    const retryAfter = response.headers.get("retry-after") ?? undefined;
    return { status: response.status, body, retryAfter };
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  } finally {
    clearTimeout(timer);
  }
};
