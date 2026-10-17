// jwtExpiryMs returns the token's exp claim in milliseconds, or 0 when it is not a readable JWT.
function jwtExpiryMs(token) {
  const payload = token.split(".")[1];
  if (!payload) return 0;
  try {
    const claims = JSON.parse(atob(payload.replace(/-/g, "+").replace(/_/g, "/")));
    return typeof claims.exp === "number" ? claims.exp * 1000 : 0;
  } catch {
    return 0;
  }
}

async function chatgptFetch(url) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
    // Reuse the page token until shortly before it expires, so a sync does not
    // add a /api/auth/session request in front of every conversation request.
    let accessToken = globalThis.__agentsviewChatGPTToken;
    if (!accessToken || Date.now() > (globalThis.__agentsviewChatGPTTokenUntil ?? 0)) {
      const session = await fetch("https://chatgpt.com/api/auth/session", {
        method: "GET",
        credentials: "include",
        redirect: "error",
        signal: controller.signal,
      });
      if (!session.ok) {
        // Forward Retry-After so a longer cooldown from the session endpoint is honored.
        return {
          status: session.status,
          body: "",
          retryAfter: session.headers.get("retry-after") ?? undefined,
        };
      }
      const auth = await session.json();
      accessToken = auth?.accessToken;
      if (typeof accessToken !== "string" || accessToken.length === 0) {
        globalThis.__agentsviewChatGPTToken = undefined;
        return { status: 401, body: "" };
      }
      globalThis.__agentsviewChatGPTToken = accessToken;
      // Keep the token only when its JWT expiry is readable; otherwise fetch it each time.
      globalThis.__agentsviewChatGPTTokenUntil = jwtExpiryMs(accessToken) - 60000;
    }

    const response = await fetch(url, {
      method: "GET",
      credentials: "include",
      redirect: "error",
      headers: { Authorization: `Bearer ${accessToken}` },
      signal: controller.signal,
    });
    if (response.status === 401) {
      // A rejected token must not be reused; the next request fetches a fresh session.
      globalThis.__agentsviewChatGPTToken = undefined;
      globalThis.__agentsviewChatGPTTokenUntil = 0;
    }
    const limit = 32 * 1024 * 1024; // Browser sync caps each response at 32 MiB.
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
    return {
      status: response.status,
      body,
      retryAfter: response.headers.get("retry-after") ?? undefined,
    };
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  } finally {
    clearTimeout(timer);
  }
}
