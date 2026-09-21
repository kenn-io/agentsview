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
const INLINE_IMAGE_MEDIA_TYPES = new Set([
  "image/png",
  "image/jpeg",
  "image/webp",
  "image/gif",
]);
const INLINE_IMAGE_DATA_URI = /^data:([^;,]+);base64,([A-Za-z0-9+/]+={0,2})$/;

function inlineImageMarkdown(imageURL: unknown): string | null {
  if (typeof imageURL !== "string") return null;
  const match = INLINE_IMAGE_DATA_URI.exec(imageURL);
  if (!match || !INLINE_IMAGE_MEDIA_TYPES.has(match[1]!)) return null;
  if (match[2]!.length % 4 !== 0) return null;
  return `![${match[1]}](${imageURL})`;
}

/** Display retained image placeholders alongside their surrounding text.
 * Unrecognized results stay intact so display projection cannot hide blocks. */
export function displayToolResult(content: string): string {
  return projectToolResult(content, "stored");
}

export function displayFormattedToolResult(content: string): string {
  return projectToolResult(content, "formatted");
}

type ToolResultDisplayMode = "stored" | "formatted";

function projectToolResult(content: string, mode: ToolResultDisplayMode): string {
  const hasPlaceholderMarker = content.includes('"agentsview_image"');
  const hasRetainedImageMarker = mode === "formatted" && content.includes('"input_image"');
  if (!hasPlaceholderMarker && !hasRetainedImageMarker) return content;
  const direct = displayToolResultArray(content, mode);
  if (direct !== null) return direct;

  // Multi-agent summaries join labelled results and trailing anonymous output
  // with blank lines. Keep each label and any unrecognized part as stored.
  return content
    .split("\n\n")
    .map((part) => {
      const newline = part.indexOf("\n");
      if (newline > 0 && part.slice(0, newline).trimEnd().endsWith(":")) {
        const body = part.slice(newline + 1);
        return part.slice(0, newline + 1) + (displayToolResultArray(body, mode) ?? body);
      }
      return displayToolResultArray(part, mode) ?? part;
    })
    .join("\n\n");
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
    if (mode === "formatted" && candidate.type === "input_image") {
      const image = inlineImageMarkdown(candidate.image_url);
      if (image === null) return content;
      hasImage = true;
      text.push(image);
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
