import type { Attachment } from "svelte/attachments";
import { reportErrorShown, type ErrorKind } from "./telemetry.js";

/** _identity makes the attachment expression react to the displayed item's identity. */
export function errorTelemetry(
  kind: ErrorKind | undefined,
  _identity?: string | null,
): Attachment<HTMLElement> {
  return () => {
    if (kind) reportErrorShown(kind);
  };
}
