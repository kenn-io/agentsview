import type { Locator, Page } from "@playwright/test";

type Rgba = [number, number, number, number];

export type Contrast = { foreground: string; background: string; ratio: number };

/** Stores the theme for the next navigation. */
export async function useTheme(page: Page, mode: "light" | "dark", highContrast = false) {
  await page.addInitScript(
    ([theme, hc]) => {
      localStorage.setItem("theme", theme as string);
      localStorage.setItem("theme-high-contrast", hc ? "true" : "false");
    },
    [mode, highContrast] as const,
  );
}

/**
 * Contrast of a rendered color against the composited background behind
 * an element. Colors resolve through a canvas, so color-mix() and alpha
 * results compare as sRGB bytes; translucent backgrounds are layered over
 * their ancestors' fills, and translucent text over that result.
 *
 * `foreground` is a CSS color or, by default, the element's own `color`.
 * `background` is a CSS color layered over the element's backdrop (for an
 * SVG shape behind text). `opacity` multiplies the text alpha (for SVG
 * fill-opacity and the like).
 */
export async function renderedContrast(
  locator: Locator,
  options: { foreground?: string; background?: string; opacity?: number } = {},
): Promise<Contrast> {
  return locator.evaluate(
    (element, { foreground, background: backdrop, opacity }) => {
      const canvas = document.createElement("canvas");
      canvas.width = canvas.height = 1;
      const context = canvas.getContext("2d", { willReadFrequently: true })!;
      const rgba = (css: string): Rgba => {
        context.clearRect(0, 0, 1, 1);
        context.fillStyle = "#000";
        context.fillStyle = css;
        context.fillRect(0, 0, 1, 1);
        const d = context.getImageData(0, 0, 1, 1).data;
        return [d[0]!, d[1]!, d[2]!, d[3]! / 255];
      };
      const over = (top: Rgba, under: Rgba): Rgba => {
        const a = top[3];
        return [0, 1, 2].map((i) => top[i]! * a + under[i]! * (1 - a)).concat(1) as Rgba;
      };
      const layers: Rgba[] = [];
      for (let node: Element | null = element; node; node = node.parentElement) {
        const fill = rgba(getComputedStyle(node).backgroundColor);
        if (fill[3] > 0) layers.push(fill);
        if (fill[3] === 1) break;
      }
      let background: Rgba = [255, 255, 255, 1];
      for (const layer of layers.reverse()) background = over(layer, background);
      if (backdrop) background = over(rgba(backdrop), background);
      const text = rgba(foreground ?? getComputedStyle(element).color);
      const ink = over([text[0], text[1], text[2], text[3] * (opacity ?? 1)], background);
      const luminance = (c: Rgba) => {
        const [r, g, b] = c.slice(0, 3).map((v) => {
          const s = v / 255;
          return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
        });
        return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
      };
      const [hi, lo] = [luminance(ink), luminance(background)].sort((x, y) => y - x);
      const hex = (c: Rgba) =>
        "#" +
        c
          .slice(0, 3)
          .map((v) => Math.round(v).toString(16).padStart(2, "0"))
          .join("");
      return {
        foreground: hex(ink),
        background: hex(background),
        ratio: Math.round(((hi! + 0.05) / (lo! + 0.05)) * 100) / 100,
      };
    },
    { foreground: options.foreground, background: options.background, opacity: options.opacity },
  );
}

/**
 * The rendered ::placeholder color of an input. Chromium's
 * getComputedStyle(input, "::placeholder") reports the input's own color,
 * so read the placeholder node in the input's UA shadow root over CDP.
 * Chromium only.
 */
export async function placeholderColor(page: Page, input: Locator): Promise<string> {
  await input.evaluate((element) => element.setAttribute("data-placeholder-probe", ""));
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send("DOM.enable");
    await cdp.send("CSS.enable");
    const { root } = await cdp.send("DOM.getDocument", { depth: -1, pierce: true });
    type DomNode = {
      nodeId: number;
      attributes?: string[];
      children?: DomNode[];
      shadowRoots?: DomNode[];
    };
    const find = (node: DomNode, test: (n: DomNode) => boolean): DomNode | null => {
      if (test(node)) return node;
      for (const child of [...(node.shadowRoots ?? []), ...(node.children ?? [])]) {
        const hit = find(child, test);
        if (hit) return hit;
      }
      return null;
    };
    const target = find(
      root as DomNode,
      (n) => n.attributes?.includes("data-placeholder-probe") ?? false,
    );
    const placeholder =
      target &&
      find(target, (n) => {
        const attrs = n.attributes ?? [];
        const at = attrs.indexOf("pseudo");
        return at >= 0 && attrs[at + 1] === "-webkit-input-placeholder";
      });
    if (!placeholder) throw new Error("input has no placeholder node");
    const { computedStyle } = await cdp.send("CSS.getComputedStyleForNode", {
      nodeId: placeholder.nodeId,
    });
    return computedStyle.find((p) => p.name === "color")!.value;
  } finally {
    await cdp.detach();
    await input.evaluate((element) => element.removeAttribute("data-placeholder-probe"));
  }
}
