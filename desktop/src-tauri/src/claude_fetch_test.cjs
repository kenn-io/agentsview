const assert = require("node:assert/strict");
const script = require("node:fs").readFileSync(0, "utf8");

(async () => {
  for (const kind of ["stream oversize", "exact limit", "split utf8", "signed out"]) {
    const limit = 32 * 1024 * 1024;
    let reads = 0;
    let cancelled = false;
    let signal;
    const calls = [];
    const authBody = require("node:fs").readFileSync(process.env.CLAUDE_SIGNED_OUT_FIXTURE, "utf8");
    const chunks = kind === "split utf8"
      ? [Uint8Array.of(0xe2), Uint8Array.of(0x82, 0xac)]
      : [new Uint8Array(limit), Uint8Array.of(65)];
    if (kind === "exact limit") chunks.pop();
    if (kind === "signed out") chunks.splice(0, chunks.length, new TextEncoder().encode(authBody));
    global.window = { __TAURI__: { core: { invoke: async (command, { payload }) => {
      assert.equal(command, "claude_auth_fetch_result");
      calls.push(payload);
    } } } };
    global.fetch = async (url, options) => {
      assert.equal(url, "https://claude.ai/api/detail");
      signal = options.signal;
      return {
        status: kind === "signed out" ? 403 : 200,
        headers: new Headers(),
        body: { getReader: () => ({
          read: async () => {
            const value = chunks[reads++];
            return value ? { value, done: false } : { done: true };
          },
          cancel: async () => { cancelled = true; assert.equal(signal.aborted, true); throw new DOMException("Aborted", "AbortError"); },
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
      assert.equal(reads, 2);
      assert.equal(cancelled, true);
    } else if (kind === "signed out") {
      assert.equal(result.status, 403);
      assert.equal(result.body, authBody);
    } else {
      assert.equal(result.status, 200);
      assert.equal(result.body, kind === "split utf8" ? "€" : "\0".repeat(limit));
    }
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
