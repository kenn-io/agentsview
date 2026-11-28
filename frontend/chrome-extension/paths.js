import { platforms } from "./platforms.js";

const requestShapes = new Map();

function shapePattern(shape, params) {
  const pattern = shape
    .split(/(\{[a-zA-Z]+\})/)
    .map((part) => {
      if (!part.startsWith("{")) return part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const name = part.slice(1, -1);
      if (!Object.hasOwn(params, name)) throw new Error(`Unknown request placeholder ${part}`);
      return params[name];
    })
    .join("");
  return new RegExp(`^${pattern}$`);
}

function shapesFor(platform) {
  if (!requestShapes.has(platform)) {
    const { params } = platforms[platform];
    const pending = fetch(chrome.runtime.getURL(`requests/${platform}.txt`))
      .then(async (response) => {
        if (!response.ok) throw new Error("Browser request shapes unavailable");
        return (await response.text())
          .trim()
          .split(/\r?\n/)
          .map((shape) => shapePattern(shape, params));
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
  if (typeof path !== "string" || !Object.hasOwn(platforms, platform)) return false;
  const shapes = await shapesFor(platform);
  return shapes.some((shape) => shape.test(path));
}
