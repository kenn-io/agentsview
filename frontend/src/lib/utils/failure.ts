import { m } from "../i18n/index.js";

/**
 * The detail to show for a failed request: the error's own message, or the
 * localized "Failed to load" when the error carries none.
 */
export function failureMessage(err: unknown): string {
  if (err instanceof Error && err.message.trim()) return err.message;
  if (typeof err === "string" && err.trim()) return err.trim();
  return m.shared_failed_to_load();
}
