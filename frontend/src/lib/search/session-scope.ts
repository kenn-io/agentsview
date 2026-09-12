/**
 * One projection of what the current transcript renders.
 *
 * The transcript components and the in-session search index both build their
 * view of a session here, so a block filter can never hide a message in the
 * DOM while search still counts or reveals it. The module stays free of store
 * imports: callers pass the current filters in and receive display items plus
 * the block kinds those items actually render.
 */
import type { Message } from "../api/types.js";
import type { BlockType } from "../stores/ui.svelte.js";
import { hasVisibleSegments } from "../utils/content-parser.js";
import { buildDisplayItems, type DisplayItem } from "../utils/display-items.js";
import { isSystemMessage } from "../utils/messages.js";
import { filterDisplayItemsByTranscriptMode } from "../utils/transcript-mode.js";
import type { SearchBlockKind } from "./block-text.js";

export interface SessionScopeInput {
  messages: readonly Message[];
  /** Transcript mode; absent means normal. */
  transcriptMode?: "normal" | "focused";
  /** Visible block types; absent means every type is visible. */
  visibleBlocks?: ReadonlySet<BlockType>;
  /** Whether any block type is hidden; absent means no filters. */
  hasBlockFilters?: boolean;
  /** Provider rule that keeps an answer before its trailing tool calls. */
  keepAnswerBeforeTrailingTools?: boolean;
}

export interface SessionScope {
  /** Display items the current transcript mode and filters render. */
  items: DisplayItem[];
  /** Display items normal mode renders under the same block filters. */
  normalItems: DisplayItem[];
  /** Messages reachable through `items`, in transcript order and deduplicated. */
  messages: Message[];
  /** Whether a searchable block kind is rendered by an included message. */
  allowsBlock(message: Message, kind: SearchBlockKind): boolean;
}

/** Look up the provider preference that focused mode respects. */
export function keepsAnswerBeforeTrailingTools(
  providers: readonly { id: string; post_answer_tool_work?: boolean | null }[],
  agentId: string | null | undefined,
): boolean {
  if (agentId == null) return false;
  return providers.some(
    (provider) => provider.id === agentId && provider.post_answer_tool_work === true,
  );
}

/** Map a searchable block kind to the filter that owns it. */
function kindVisible(
  isVisible: (type: BlockType) => boolean,
  role: Message["role"],
  kind: SearchBlockKind,
): boolean {
  switch (kind) {
    case "thinking":
      return isVisible("thinking");
    case "code":
      return isVisible("code");
    case "tool-input":
    case "tool-output":
    case "tool-history":
      return isVisible("tool");
    case "text":
    case "skill":
      return isVisible(role === "user" ? "user" : "assistant");
  }
}

export function projectSessionScope(input: SessionScopeInput): SessionScope {
  const visibleBlocks = input.visibleBlocks;
  const isVisible = (type: BlockType): boolean =>
    visibleBlocks === undefined || visibleBlocks.has(type);
  const hasBlockFilters = input.hasBlockFilters ?? false;
  const transcriptMode = input.transcriptMode ?? "normal";

  const baseMessages = input.messages.filter((message) => !isSystemMessage(message));
  const baseItems = buildDisplayItems(baseMessages);
  const filteredItems = buildDisplayItems(baseMessages, {
    skipToolGrouping: !isVisible("tool"),
  });
  const itemVisible = (item: DisplayItem): boolean =>
    item.kind === "tool-group" || hasVisibleSegments(item.message, isVisible);

  const normalItems = hasBlockFilters ? filteredItems.filter(itemVisible) : baseItems;
  let items: DisplayItem[];
  if (transcriptMode === "normal") {
    items = normalItems;
  } else if (!hasBlockFilters) {
    items = filterDisplayItemsByTranscriptMode(baseItems, "focused", {
      keepAnswerBeforeTrailingTools: input.keepAnswerBeforeTrailingTools,
    });
  } else {
    items = filterDisplayItemsByTranscriptMode(filteredItems, "focused", {
      keepAnswerBeforeTrailingTools: input.keepAnswerBeforeTrailingTools,
      isMessageVisible: (message) => hasVisibleSegments(message, isVisible),
    }).filter(itemVisible);
  }

  const messages: Message[] = [];
  const included = new Set<Message>();
  for (const item of items) {
    const group = item.kind === "tool-group" ? item.messages : [item.message];
    for (const message of group) {
      if (included.has(message)) continue;
      included.add(message);
      messages.push(message);
    }
  }

  return {
    items,
    normalItems,
    messages,
    allowsBlock: (message, kind) =>
      included.has(message) && kindVisible(isVisible, message.role, kind),
  };
}
