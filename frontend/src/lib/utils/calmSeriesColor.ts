// Usage charts paint large areas, so full-chroma palette colors read as loud
// blocks. calmSeriesColor lowers a color's OKLCH chroma while keeping its
// lightness and hue, so each category keeps its hue in both themes. Non-hex
// colors pass through unchanged; resolve CSS variables first with
// resolveRootColor.

export const CALM_CHROMA_SCALE = 0.8;

function toLinear(channel: number): number {
  return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
}

function fromLinear(linear: number): number {
  const channel = Math.min(1, Math.max(0, linear));
  return channel <= 0.0031308 ? channel * 12.92 : 1.055 * channel ** (1 / 2.4) - 0.055;
}

function toHexByte(channel: number): string {
  const byte = Math.round(Math.min(1, Math.max(0, channel)) * 255);
  return byte.toString(16).padStart(2, "0");
}

export function calmSeriesColor(color: string, scale = CALM_CHROMA_SCALE): string {
  const match = /^#([0-9a-f]{6})$/i.exec(color.trim());
  if (!match) return color;
  const value = Number.parseInt(match[1]!, 16);
  const r = toLinear(((value >> 16) & 0xff) / 255);
  const g = toLinear(((value >> 8) & 0xff) / 255);
  const b = toLinear((value & 0xff) / 255);

  // Linear sRGB to OKLab (Bjorn Ottosson's matrices).
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  const lightness = 0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s;
  const a = (1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s) * scale;
  const bAxis = (0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s) * scale;

  // Scaling a and b together lowers chroma and leaves hue unchanged.
  const l2 = (lightness + 0.3963377774 * a + 0.2158037573 * bAxis) ** 3;
  const m2 = (lightness - 0.1055613458 * a - 0.0638541728 * bAxis) ** 3;
  const s2 = (lightness - 0.0894841775 * a - 1.291485548 * bAxis) ** 3;
  const outR = 4.0767416621 * l2 - 3.3077115913 * m2 + 0.2309699292 * s2;
  const outG = -1.2684380046 * l2 + 2.6097574011 * m2 - 0.3413193965 * s2;
  const outB = -0.0041960863 * l2 - 0.7034186147 * m2 + 1.707815448 * s2;
  return `#${toHexByte(fromLinear(outR))}${toHexByte(fromLinear(outG))}${toHexByte(fromLinear(outB))}`;
}

/** The value of a `var(--name)` color on the document root, or the color
 * itself when it is not a bare variable or the variable is unset. */
export function resolveRootColor(color: string): string {
  const name = /^var\((--[\w-]+)\)$/.exec(color.trim())?.[1];
  if (!name || typeof document === "undefined") return color;
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || color;
}
