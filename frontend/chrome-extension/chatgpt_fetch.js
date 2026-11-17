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

// chatgptToken returns the page's access token, reusing the cached one until
// shortly before it expires so a sync does not add a /api/auth/session request
// in front of every conversation request. It returns a response instead when
// the session request fails or the page has no token.
async function chatgptToken(refresh) {
  const cached = globalThis.__agentsviewChatGPTToken;
  if (!refresh && cached && Date.now() <= (globalThis.__agentsviewChatGPTTokenUntil ?? 0)) {
    return { token: cached, cached: true };
  }
  globalThis.__agentsviewChatGPTToken = undefined;
  globalThis.__agentsviewChatGPTTokenUntil = 0;
  const session = await globalThis.pageFetch("https://chatgpt.com/api/auth/session");
  if (session.status < 200 || session.status >= 300) return { response: session };
  const accessToken = JSON.parse(session.body)?.accessToken;
  if (typeof accessToken !== "string" || accessToken.length === 0) {
    return { response: { status: 401, body: "" } };
  }
  globalThis.__agentsviewChatGPTToken = accessToken;
  // Keep the token only when its JWT expiry is readable; otherwise fetch it each time.
  globalThis.__agentsviewChatGPTTokenUntil = jwtExpiryMs(accessToken) - 60000;
  return { token: accessToken, cached: false };
}

async function chatgptFetch(url) {
  try {
    for (const refresh of [false, true]) {
      const auth = await chatgptToken(refresh);
      if (auth.response) return auth.response;
      const response = await globalThis.pageFetch(url, { Authorization: `Bearer ${auth.token}` });
      if (response.status !== 401) return response;
      // A rejected token is never reused. A cached one may have been revoked
      // before its expiry, so fetch the session once more; a fresh token the
      // server rejects means the sign-in expired, which Sync reports.
      globalThis.__agentsviewChatGPTToken = undefined;
      globalThis.__agentsviewChatGPTTokenUntil = 0;
      if (!auth.cached) return response;
    }
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  }
}
