import { parseHexColor } from "@kenn-io/kit-ui";
import {
  COLORBLIND_COLORS,
  MESSAGE_ROLES,
  parseStoredColors,
  roleColorProperties,
  roleColorPropertyNames,
  type MessageColorPreset,
  type MessageColors,
  type MessageRole,
} from "../utils/messageColors.js";

const STORAGE_KEY = "agentsview-message-colors";
// Root attribute that switches the role bars from color alone to distinct
// line patterns (see app.css).
const PATTERNS_ATTRIBUTE = "data-message-role-patterns";

interface StoredMessageColors {
  colors: MessageColors;
  patterns: boolean;
}

function readStored(): StoredMessageColors {
  try {
    const raw = localStorage?.getItem(STORAGE_KEY);
    if (raw) {
      const parsed = JSON.parse(raw);
      return {
        colors: parseStoredColors(parsed?.colors),
        patterns: parsed?.patterns === true,
      };
    }
  } catch {
    // ignore
  }
  return { colors: {}, patterns: false };
}

function sameColors(a: MessageColors, b: MessageColors): boolean {
  return MESSAGE_ROLES.every((role) => a[role] === b[role]);
}

/** Per-role message colors and pattern markers, persisted in localStorage
 * like the other appearance settings and applied as root CSS properties. */
class MessageColorsStore {
  colors: MessageColors = $state({});
  patterns: boolean = $state(false);

  constructor() {
    const stored = readStored();
    this.colors = stored.colors;
    this.patterns = stored.patterns;
    $effect.root(() => {
      $effect(() => {
        const state: StoredMessageColors = { colors: this.colors, patterns: this.patterns };
        try {
          if (this.isDefault) localStorage?.removeItem(STORAGE_KEY);
          else localStorage?.setItem(STORAGE_KEY, JSON.stringify(state));
        } catch {
          // ignore
        }
        this.apply(state);
      });
    });
  }

  get isDefault(): boolean {
    return !this.patterns && MESSAGE_ROLES.every((role) => !this.colors[role]);
  }

  /** The preset the current settings match, or "custom". */
  get preset(): MessageColorPreset | "custom" {
    if (this.isDefault) return "default";
    if (this.patterns && sameColors(this.colors, COLORBLIND_COLORS)) return "colorblind";
    return "custom";
  }

  setColor(role: MessageRole, color: string): void {
    const hex = parseHexColor(color);
    if (hex === null) return;
    this.colors = { ...this.colors, [role]: hex };
  }

  resetColor(role: MessageRole): void {
    const { [role]: _removed, ...rest } = this.colors;
    this.colors = rest;
  }

  setPatterns(enabled: boolean): void {
    this.patterns = enabled;
  }

  applyPreset(preset: MessageColorPreset): void {
    if (preset === "default") {
      this.colors = {};
      this.patterns = false;
      return;
    }
    this.colors = { ...COLORBLIND_COLORS };
    this.patterns = true;
  }

  private apply({ colors, patterns }: StoredMessageColors): void {
    if (typeof document === "undefined") return;
    const root = document.documentElement;
    for (const role of MESSAGE_ROLES) {
      for (const name of roleColorPropertyNames(role)) root.style.removeProperty(name);
      const color = colors[role];
      if (!color) continue;
      for (const [name, value] of Object.entries(roleColorProperties(role, color))) {
        root.style.setProperty(name, value);
      }
    }
    root.toggleAttribute(PATTERNS_ATTRIBUTE, patterns);
  }
}

export const messageColors = new MessageColorsStore();
