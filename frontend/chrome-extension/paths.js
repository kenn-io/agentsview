const requestShapes = new Map();

const shapeFiles = {
  "claude-ai": "claude_ai_requests.txt",
  chatgpt: "chatgpt_requests.txt",
};

async function shapesFor(platform) {
  if (!Object.hasOwn(shapeFiles, platform)) return undefined;
  const file = shapeFiles[platform];
  if (!file) return undefined;
  if (!requestShapes.has(platform)) {
    const pending = fetch(chrome.runtime.getURL(file))
      .then(async (response) => {
        if (!response.ok) throw new Error("Browser request shapes unavailable");
        return (await response.text()).trim().split(/\r?\n/);
      })
      .catch((error) => {
        requestShapes.delete(platform);
        throw error;
      });
    requestShapes.set(platform, pending);
  }
  return requestShapes.get(platform);
}

export async function allowedPath(platform, path) {
  if (typeof path !== "string") return false;
  const shapes = await shapesFor(platform);
  if (!shapes) return false;
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  if (platform === "claude-ai") {
    if (path === shapes[0]) return true;
    const [prefix, list] = shapes[1].split("{organization}");
    if (!path.startsWith(prefix)) return false;
    const rest = path.slice(prefix.length);
    const slash = rest.indexOf("/");
    if (!uuid.test(rest.slice(0, slash))) return false;
    const tail = rest.slice(slash);
    const listPrefix = list.replace("{offset}", "");
    if (tail.startsWith(listPrefix) && /^\d+$/.test(tail.slice(listPrefix.length))) return true;
    const detail = shapes[2].split("{organization}")[1];
    const [detailPrefix, suffix] = detail.split("{conversation}");
    return (
      tail.startsWith(detailPrefix) &&
      tail.endsWith(suffix) &&
      uuid.test(tail.slice(detailPrefix.length, -suffix.length))
    );
  }
  if (platform === "chatgpt") {
    const [listPrefix, listSuffix] = shapes[0].split("{offset}");
    if (path.startsWith(listPrefix) && path.endsWith(listSuffix)) {
      const offset = path.slice(listPrefix.length, -listSuffix.length);
      return /^\d+$/.test(offset);
    }
    const [detailPrefix, detailSuffix = ""] = shapes[1].split("{conversation}");
    const conversation = detailSuffix
      ? path.slice(detailPrefix.length, -detailSuffix.length)
      : path.slice(detailPrefix.length);
    return path.startsWith(detailPrefix) && path.endsWith(detailSuffix) && uuid.test(conversation);
  }
  return false;
}
