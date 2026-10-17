import type { Attachment } from "svelte/attachments";
import { reportErrorShown, type ErrorKind } from "./telemetry.js";

/** Count each display of an error; identities stay local to the view. */
export function errorTelemetry(
  kind: ErrorKind | undefined,
  _identity?: string | null,
): Attachment<HTMLElement> {
  return () => {
    if (kind) reportErrorShown(kind);
  };
}
