import brand from "@kenn-io/kit-ui/brand.json";
import {
  contrastRatio,
  mixHexColors,
  parseHexColor,
  readableInk,
  WCAG_AA_NON_TEXT,
  WCAG_AA_TEXT,
} from "@kenn-io/kit-ui";

/** Message parts whose color the reader can change in Settings > Appearance. */
export type MessageRole = "user" | "assistant" | "thinking" | "tool" | "code";

export const MESSAGE_ROLES: readonly MessageRole[] = [
  "user",
  "assistant",
  "thinking",
  "tool",
  "code",
];

export type MessageColors = Partial<Record<MessageRole, string>>;

export type MessageColorPreset = "default" | "colorblind";

/**
 * Okabe-Ito hues, the colors behind matplotlib's seaborn-v0_8-colorblind
 * style. User, assistant, and tool use them unchanged. Thinking and code
 * darken its pink (#cc79a7) and yellow (#f0e442), which fail the contrast
 * checks below on light surfaces. Blue against vermillion is the pair that
 * stays apart under all common forms of color blindness, so it marks the
 * user and the assistant.
 */
export const COLORBLIND_COLORS: Readonly<Record<MessageRole, string>> = {
  user: "#0072b2",
  assistant: "#d55e00",
  thinking: "#b8689a",
  tool: "#009e73",
  code: "#8f7d00",
};

export type ContrastMode = "light" | "dark" | "high-contrast-light" | "high-contrast-dark";

export const CONTRAST_MODES: readonly ContrastMode[] = [
  "light",
  "dark",
  "high-contrast-light",
  "high-contrast-dark",
];

// Mirrors --message-role-tint in app.css: how much of a custom role color
// tints its message background.
const ROLE_TINT = { light: 0.08, dark: 0.14 };
// kit-ui's tone recipe: label text is the role color mixed 72% toward the
// theme's text color, which keeps small text legible on the tint.
const INK_WEIGHT = 0.72;
// Mirror --code-bg in app.css.
const CODE_BG = { light: "#1e1e2e", dark: "#0d0d14" };

interface ModePalette {
  surface: string;
  text: string;
  textSecondary: string;
  tint: number;
  defaults: Record<MessageRole, string>;
}

function modePalette(mode: ContrastMode): ModePalette {
  const dark = mode === "dark" || mode === "high-contrast-dark";
  const base = dark ? brand.colors.dark : brand.colors.light;
  const colors =
    mode === "high-contrast-light"
      ? { ...base, ...brand.colors.highContrastLight }
      : mode === "high-contrast-dark"
        ? { ...base, ...brand.colors.highContrastDark }
        : base;
  return {
    surface: colors.backgroundSurface,
    text: colors.textPrimary,
    textSecondary: colors.textSecondary,
    tint: dark ? ROLE_TINT.dark : ROLE_TINT.light,
    // The theme tokens each role uses when the reader has not picked a
    // color: --accent-blue, --accent-purple, --accent-amber, --code-bg.
    defaults: {
      user: colors.action,
      assistant: colors.purple,
      thinking: colors.purple,
      tool: colors.warning,
      code: dark ? CODE_BG.dark : CODE_BG.light,
    },
  };
}

/** The color a role shows in `mode` when the reader has not picked one. */
export function defaultRoleColor(role: MessageRole, mode: ContrastMode): string {
  return modePalette(mode).defaults[role];
}

/** What a low-contrast pick makes hard to perceive. */
export type ColorIssueKind = "text" | "marker";

export interface ColorIssue {
  kind: ColorIssueKind;
  /** Modes where the pick falls short, in CONTRAST_MODES order. */
  modes: ContrastMode[];
  /** The worst ratio across those modes. */
  ratio: number;
  required: number;
}

/**
 * Checks a role color against every theme mode. Text covers the role label,
 * drawn in the mixed ink, and body text on the tinted background. Marker
 * covers the accent bar, or the frame around code blocks, which must stand
 * out from what surrounds it.
 */
export function colorIssues(role: MessageRole, color: string): ColorIssue[] {
  const hex = parseHexColor(color);
  if (hex === null) return [];
  const failures: Record<
    ColorIssueKind,
    { mode: ContrastMode; ratio: number; required: number }[]
  > = { text: [], marker: [] };
  for (const mode of CONTRAST_MODES) {
    const palette = modePalette(mode);
    if (role === "code") {
      const ratio = contrastRatio(hex, palette.surface);
      if (ratio < WCAG_AA_NON_TEXT) {
        failures.marker.push({ mode, ratio, required: WCAG_AA_NON_TEXT });
      }
      continue;
    }
    const background = mixHexColors(hex, palette.surface, palette.tint);
    const textRatio = Math.min(
      contrastRatio(mixHexColors(hex, palette.text, INK_WEIGHT), background),
      contrastRatio(palette.textSecondary, background),
    );
    if (textRatio < WCAG_AA_TEXT) {
      failures.text.push({ mode, ratio: textRatio, required: WCAG_AA_TEXT });
    }
    const markerRatio = contrastRatio(hex, background);
    if (markerRatio < WCAG_AA_NON_TEXT) {
      failures.marker.push({ mode, ratio: markerRatio, required: WCAG_AA_NON_TEXT });
    }
  }
  return (["text", "marker"] as const)
    .filter((kind) => failures[kind].length > 0)
    .map((kind) => ({
      kind,
      modes: failures[kind].map((failure) => failure.mode),
      ratio: Math.min(...failures[kind].map((failure) => failure.ratio)),
      required: failures[kind][0]!.required,
    }));
}

const BACKGROUND_TOKEN: Record<Exclude<MessageRole, "code">, string> = {
  user: "--user-bg",
  assistant: "--assistant-bg",
  thinking: "--thinking-bg",
  tool: "--tool-bg",
};

/** Root CSS custom properties that apply a custom role color. Properties
 * reference theme tokens, so one pick follows light, dark, and high
 * contrast. */
export function roleColorProperties(role: MessageRole, color: string): Record<string, string> {
  const hex = parseHexColor(color);
  if (hex === null) return {};
  if (role === "code") {
    return { "--role-code-accent": hex, "--role-code-frame-width": "2px" };
  }
  return {
    ...(role === "tool" ? { "--role-tool-group-rail": hex } : {}),
    [`--role-${role}-accent`]: hex,
    [`--role-${role}-accent-foreground`]: readableInk(hex, ["#000000", "#ffffff"]),
    [`--role-${role}-ink`]: `color-mix(in srgb, ${hex} ${INK_WEIGHT * 100}%, var(--text-primary))`,
    [BACKGROUND_TOKEN[role]]:
      `color-mix(in srgb, ${hex} var(--message-role-tint), var(--bg-surface))`,
  };
}

/** Every property roleColorProperties can set, so a reset can clear them. */
export function roleColorPropertyNames(role: MessageRole): string[] {
  return Object.keys(roleColorProperties(role, "#000000"));
}

/** Reads stored colors, dropping unknown roles and malformed values. */
export function parseStoredColors(value: unknown): MessageColors {
  if (!value || typeof value !== "object") return {};
  const colors: MessageColors = {};
  for (const role of MESSAGE_ROLES) {
    const raw = (value as Record<string, unknown>)[role];
    const hex = typeof raw === "string" ? parseHexColor(raw) : null;
    if (hex) colors[role] = hex;
  }
  return colors;
}
