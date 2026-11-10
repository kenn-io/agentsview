// worker.js injects claude_fetch.js first. Gemini reads through batchexecute
// POSTs, so the two allowed request shapes carry named parameters that
// geminiFetch turns into each RPC's argument. The page's request token stays
// in the page.
const geminiPageSize = 20;
const geminiArguments = {
  MaZiqc: (query) =>
    query.get("cursor") ? [geminiPageSize, query.get("cursor")] : [geminiPageSize],
  hNvQHb: (query) => [
    query.get("chat"),
    geminiPageSize,
    query.get("cursor") || null,
    1,
    [0],
    [4],
    null,
    1,
  ],
};

// geminiPageValue reads one "key":"value" string from the app page's inline data.
function geminiPageValue(html, key) {
  const start = html.indexOf(`"${key}":"`);
  if (start < 0) return "";
  let raw = "";
  for (let i = start + key.length + 4; i < html.length && html[i] !== '"'; i++) {
    raw += html[i];
    if (html[i] === "\\") raw += html[++i];
  }
  try {
    return JSON.parse(`"${raw}"`);
  } catch {
    return "";
  }
}

// geminiSession returns the request token and build identifiers, reading the
// open tab first and the app page when the tab has none or the token was refused.
async function geminiSession(account, refresh) {
  if (!refresh && globalThis.__agentsviewGemini) return globalThis.__agentsviewGemini;
  globalThis.__agentsviewGemini = undefined;
  let html = refresh ? "" : document.documentElement.innerHTML;
  if (!geminiPageValue(html, "SNlM0e")) {
    const page = await globalThis.claudeFetch(`${location.origin}${account}/app`);
    if (page.status < 200 || page.status >= 300) return page;
    html = page.body;
  }
  const session = {
    at: geminiPageValue(html, "SNlM0e"),
    sid: geminiPageValue(html, "FdrFJe"),
    bl: geminiPageValue(html, "cfb2h"),
  };
  if (!session.at) return { status: 401, body: "" };
  globalThis.__agentsviewGemini = session;
  return session;
}

async function geminiFetch(url) {
  try {
    const request = new URL(url);
    const rpc = request.searchParams.get("rpcids");
    if (!Object.hasOwn(geminiArguments, rpc))
      return { status: 0, body: "", error: "Unsupported Gemini request" };
    const account = location.pathname.match(/^\/u\/\d+(?=\/)/)?.[0] ?? "";
    for (const refresh of [false, true]) {
      const session = await geminiSession(account, refresh);
      if (!session.at) return session;
      const target = new URL(`${request.origin}${account}/_/BardChatUi/data/batchexecute`);
      target.searchParams.set("rpcids", rpc);
      target.searchParams.set("source-path", "/app");
      target.searchParams.set("_reqid", String(100000 + Math.floor(Math.random() * 900000)));
      target.searchParams.set("rt", "c");
      if (session.bl) target.searchParams.set("bl", session.bl);
      if (session.sid) target.searchParams.set("f.sid", session.sid);
      const body = new URLSearchParams();
      body.set(
        "f.req",
        JSON.stringify([
          [[rpc, JSON.stringify(geminiArguments[rpc](request.searchParams)), null, "generic"]],
        ]),
      );
      body.set("at", session.at);
      const response = await globalThis.claudeFetch(
        target.toString(),
        { "Content-Type": "application/x-www-form-urlencoded;charset=utf-8", "X-Same-Domain": "1" },
        body.toString(),
      );
      // A refused token is fetched again once; a second refusal reaches Sync.
      if (refresh || (response.status !== 400 && response.status !== 401)) {
        if (response.status === 400 || response.status === 401)
          globalThis.__agentsviewGemini = undefined;
        return response;
      }
    }
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  }
}
