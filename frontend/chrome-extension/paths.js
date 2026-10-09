let requestShapes;

export async function allowedPath(path) {
  if (typeof path !== "string") return false;
  requestShapes ??= fetch(chrome.runtime.getURL("claude_ai_requests.txt")).then(async (response) => {
    if (!response.ok) throw new Error("Claude request shapes unavailable");
    return (await response.text()).trim().split(/\r?\n/);
  });
  const shapes = await requestShapes;
  if (path === shapes[0]) return true;
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
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
  return tail.startsWith(detailPrefix) && tail.endsWith(suffix) && uuid.test(tail.slice(detailPrefix.length, -suffix.length));
}
