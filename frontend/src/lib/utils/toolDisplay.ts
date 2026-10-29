/** Returns the user-facing label for a tool call.
 *
 *  Prefers the normalized category (e.g. "Bash" for codex's
 *  exec_command) over the raw tool name, so analytics and
 *  ToolBlock headers stay consistent across agents.
 *
 *  Falls back to the raw tool name when the category is too
 *  generic ("Other") or when a tool's specific name is more
 *  informative than its category (e.g. "Skill" inside the
 *  Tool category). */
export function displayToolName(call: { tool_name: string; category?: string | null }): string {
  const cat = call.category;
  if (cat && cat !== "Other" && cat !== "Tool") return cat;
  return call.tool_name;
}

// Keep this list aligned with internal/assets/assets.go mediaTypeToExt.
const INLINE_IMAGE_MEDIA_TYPES = new Set(["image/png", "image/jpeg", "image/webp", "image/gif"]);
const INLINE_IMAGE_DATA_URI = /^data:([^;,]+);base64,([A-Za-z0-9+/]+={0,2})$/;

interface InlineImage {
  url: string;
  mediaType: string;
  byteSize: number;
}

function inlineImage(imageURL: unknown): InlineImage | null {
  if (typeof imageURL !== "string") return null;
  const match = INLINE_IMAGE_DATA_URI.exec(imageURL);
  if (!match || !INLINE_IMAGE_MEDIA_TYPES.has(match[1]!)) return null;
  const payload = match[2]!;
  if (payload.length % 4 !== 0) return null;
  const padding = payload.endsWith("==") ? 2 : payload.endsWith("=") ? 1 : 0;
  return { url: imageURL, mediaType: match[1]!, byteSize: (payload.length / 4) * 3 - padding };
}

/** Display retained image placeholders alongside their surrounding text.
 * Unrecognized results stay intact so display projection cannot hide blocks. */
export function displayToolResult(content: string): string {
  return projectToolResult(content, "stored");
}

export function displayFormattedToolResult(content: string): string {
  return projectToolResult(content, "formatted");
}

/** One-line previews name retained images with the archive's drop-mode
 * placeholder text instead of showing their base64 payload. */
export function displayToolResultPreview(content: string): string {
  return projectToolResult(content, "preview");
}

type ToolResultDisplayMode = "stored" | "formatted" | "preview";

function projectToolResult(content: string, mode: ToolResultDisplayMode): string {
  const hasPlaceholderMarker = content.includes('"agentsview_image"');
  const hasRetainedImageMarker = mode !== "stored" && content.includes('"input_image"');
  if (!hasPlaceholderMarker && !hasRetainedImageMarker) return content;
  const direct = displayToolResultArray(content, mode);
  if (direct !== null) return direct;

  // Keep each label and any unrecognized part as stored.
  return summarySections(content)
    .map((part) => {
      const [label, body] = splitSectionLabel(part);
      return label + (displayToolResultArray(body, mode) ?? body);
    })
    .join("\n\n");
}

// Multi-agent summaries join labelled results and trailing anonymous output
// with blank lines, and a result's JSON can hold blank lines of its own. Like
// the archive's scanSummarySections, rejoin pieces until a section's JSON
// parses. Joining the sections with blank lines restores the content.
function summarySections(content: string): string[] {
  const pieces = content.split("\n\n");
  const join = (from: number, to: number) => pieces.slice(from, to).join("\n\n");
  const sections: string[] = [];
  for (let start = 0; start < pieces.length;) {
    let end = start + 1;
    const first = join(start, end);
    if (splitSectionLabel(first)[1].trimStart().startsWith("[") && !parsesAsJSON(first)) {
      for (let to = end + 1; to <= pieces.length; to++) {
        if (parsesAsJSON(join(start, to))) {
          end = to;
          break;
        }
      }
    }
    sections.push(join(start, end));
    start = end;
  }
  return sections;
}

/** Splits an "agent:" label line from a summary section's body. */
function splitSectionLabel(part: string): [string, string] {
  const newline = part.indexOf("\n");
  if (newline > 0 && part.slice(0, newline).trimEnd().endsWith(":")) {
    return [part.slice(0, newline + 1), part.slice(newline + 1)];
  }
  return ["", part];
}

function parsesAsJSON(section: string): boolean {
  try {
    JSON.parse(splitSectionLabel(section)[1]);
    return true;
  } catch {
    return false;
  }
}

function displayToolResultArray(content: string, mode: ToolResultDisplayMode): string | null {
  let blocks: unknown;
  try {
    blocks = JSON.parse(content);
  } catch {
    return null;
  }
  if (!Array.isArray(blocks)) return content;
  const text: string[] = [];
  let hasImage = false;
  for (const block of blocks) {
    if (!block || typeof block !== "object") {
      return content;
    }
    const candidate = block as {
      type?: unknown;
      version?: unknown;
      text?: unknown;
      image_url?: unknown;
    };
    if (mode !== "stored" && candidate.type === "input_image") {
      const image = inlineImage(candidate.image_url);
      if (image === null) return content;
      hasImage = true;
      text.push(
        mode === "formatted"
          ? `![${image.mediaType}](${image.url})`
          : `[Image: ${image.mediaType}, ${image.byteSize} bytes]`,
      );
    } else if (typeof candidate.text !== "string") {
      return content;
    } else if (candidate.type === "agentsview_image" && candidate.version === 1) {
      hasImage = true;
      text.push(candidate.text);
    } else if (candidate.type !== "input_text" && candidate.type !== "text") {
      return content;
    } else {
      text.push(candidate.text);
    }
  }
  return hasImage ? text.join("\n\n") : content;
}

/** Image sources a tool result holds, in block order: inline data URIs for
 * retained images and asset:// references for offloaded ones. Dropped images
 * have no source. */
export function toolResultImageSources(content: string): string[] {
  if (!content.includes('"input_image"') && !content.includes('"image_ref"')) return [];
  return toolResultBlockArrays(content).flatMap((blocks) =>
    blocks.map(toolResultImageSource).filter((src) => src !== null),
  );
}

function toolResultImageSource(block: unknown): string | null {
  if (!block || typeof block !== "object") return null;
  const candidate = block as { type?: unknown; image_url?: unknown; image_ref?: unknown };
  if (candidate.type === "input_image") return inlineImage(candidate.image_url)?.url ?? null;
  if (
    candidate.type === "agentsview_image" &&
    typeof candidate.image_ref === "string" &&
    candidate.image_ref.startsWith("asset://")
  ) {
    return candidate.image_ref;
  }
  return null;
}

// A whole-result array, or the arrays of a multi-agent summary's sections.
function toolResultBlockArrays(content: string): unknown[][] {
  const direct = parseBlockArray(content);
  if (direct !== null) return [direct];
  return summarySections(content).flatMap((part) => {
    const blocks = parseBlockArray(splitSectionLabel(part)[1]);
    return blocks === null ? [] : [blocks];
  });
}

function parseBlockArray(content: string): unknown[] | null {
  try {
    const parsed: unknown = JSON.parse(content);
    return Array.isArray(parsed) ? parsed : null;
  } catch {
    return null;
  }
}
