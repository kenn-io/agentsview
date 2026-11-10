const requestShapes = new Map();

const shapeFiles = {
  "claude-ai": "claude_ai_requests.txt",
  chatgpt: "chatgpt_requests.txt",
};

async function shapesFor(platform) {
  if (!Object.hasOwn(shapeFiles, platform)) return undefined;
  const file = shapeFiles[platform];
  if (!requestShapes.has(platform)) {
    const pending = fetch(chrome.runtime.getURL(file))
      .then(async (response) => {
        if (!response.ok) throw new Error("Browser request shapes unavailable");
        return (await response.text()).trim().split(/\r?\n/).map(shapePattern);
      })
      .catch((error) => {
        requestShapes.delete(platform);
        throw error;
      });
    requestShapes.set(platform, pending);
  }
  return requestShapes.get(platform);
}

const uuid = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
const parameters = {
  organization: uuid,
  conversation: uuid,
  offset: "[0-9]+",
  archived: "(?:false|true)",
};

function shapePattern(shape) {
  const pattern = shape
    .split(/(\{[a-z]+\})/)
    .map((part) => {
      if (part.startsWith("{")) return parameters[part.slice(1, -1)];
      return part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    })
    .join("");
  return new RegExp(`^${pattern}$`);
}

export async function allowedPath(platform, path) {
  if (typeof path !== "string") return false;
  const shapes = await shapesFor(platform);
  return shapes?.some((shape) => shape.test(path)) ?? false;
}
