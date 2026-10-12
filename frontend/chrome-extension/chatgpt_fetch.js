async function chatgptFetch(url) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
    const session = await fetch("https://chatgpt.com/api/auth/session", {
      method: "GET",
      credentials: "include",
      redirect: "error",
      signal: controller.signal,
    });
    if (!session.ok) return { status: session.status, body: "" };
    const auth = await session.json();
    const accessToken = auth?.accessToken;
    if (typeof accessToken !== "string" || accessToken.length === 0) {
      return { status: 401, body: "" };
    }

    const response = await fetch(url, {
      method: "GET",
      credentials: "include",
      redirect: "error",
      headers: { Authorization: `Bearer ${accessToken}` },
      signal: controller.signal,
    });
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
