const assert = require("node:assert/strict");
const script = require("node:fs").readFileSync(0, "utf8");

(async () => {
  for (const kind of ["declared oversize", "stream oversize", "exact limit", "split utf8"]) {
    const limit = 32 * 1024 * 1024;
    let reads = 0;
    let cancelled = false;
    let signal;
    const calls = [];
    const chunks = kind === "split utf8"
      ? [Uint8Array.of(0xe2), Uint8Array.of(0x82, 0xac)]
      : [new Uint8Array(limit), Uint8Array.of(65)];
    if (kind === "exact limit") chunks.pop();
    global.window = { __TAURI__: { core: { invoke: async (command, { payload }) => {
      assert.equal(command, "claude_auth_fetch_result");
      calls.push(payload);
    } } } };
    global.fetch = async (url, options) => {
      assert.equal(url, "https://claude.ai/api/detail");
      signal = options.signal;
      return {
        status: 200,
        headers: new Headers(kind === "declared oversize" ? { "Content-Length": limit + 1 } : {}),
        body: { getReader: () => ({
          read: async () => {
            const value = chunks[reads++];
            return value ? { value, done: false } : { done: true };
          },
          cancel: async () => { cancelled = true; },
        }) },
      };
    };
    await eval(script);
    assert.equal(calls.length, 1, kind);
    const result = calls[0];
    assert.equal(result.requestId, "request");
    if (kind.includes("oversize")) {
      assert.equal(result.status, 413);
      assert.equal(result.body, "");
      assert.equal(signal.aborted, true);
      assert.equal(reads, kind === "declared oversize" ? 0 : 2);
      assert.equal(cancelled, kind === "stream oversize");
    } else {
      assert.equal(result.status, 200);
      assert.equal(result.body, kind === "split utf8" ? "€" : "\0".repeat(limit));
    }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
