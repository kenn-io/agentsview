import { m } from "../../i18n/index.js";
import { READING_WIDTHS, type ReadingWidth } from "../../stores/ui.svelte.js";

const LABELS: Record<ReadingWidth, () => string> = {
  default: m.appearance_reading_width_default,
  wider: m.appearance_reading_width_wider,
  full: m.appearance_reading_width_full,
};

export function readingWidthOptions(): { value: ReadingWidth; label: string }[] {
  return READING_WIDTHS.map((value) => ({ value, label: LABELS[value]() }));
}
