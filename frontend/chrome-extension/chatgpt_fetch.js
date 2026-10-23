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
  try {
    // Reuse the page token until shortly before it expires, so a sync does not
    // add a /api/auth/session request in front of every conversation request.
    let accessToken = globalThis.__agentsviewChatGPTToken;
    if (!accessToken || Date.now() > (globalThis.__agentsviewChatGPTTokenUntil ?? 0)) {
      const session = await globalThis.claudeFetch("https://chatgpt.com/api/auth/session");
      if (session.status < 200 || session.status >= 300) return session;
      const auth = JSON.parse(session.body);
      accessToken = auth?.accessToken;
      if (typeof accessToken !== "string" || accessToken.length === 0) {
        globalThis.__agentsviewChatGPTToken = undefined;
        return { status: 401, body: "" };
      }
      globalThis.__agentsviewChatGPTToken = accessToken;
      // Keep the token only when its JWT expiry is readable; otherwise fetch it each time.
      globalThis.__agentsviewChatGPTTokenUntil = jwtExpiryMs(accessToken) - 60000;
    }

    const response = await globalThis.claudeFetch(url, { Authorization: `Bearer ${accessToken}` });
    if (response.status === 401) {
      // A rejected token must not be reused; the next request fetches a fresh session.
      globalThis.__agentsviewChatGPTToken = undefined;
      globalThis.__agentsviewChatGPTTokenUntil = 0;
    }
    return response;
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  }
}
