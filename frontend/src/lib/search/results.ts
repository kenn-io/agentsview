/** Group model matches without consulting mounted or collapsed DOM content. */
import type { Message } from "../api/types.js";
import type { SearchBlock } from "./block-text.js";
import type { Match } from "./session-index.js";

export interface MatchSnippet {
  before: string;
  hit: string;
  after: string;
  leading: boolean;
  trailing: boolean;
}
export interface FindResultEntry {
  match: Match;
  block: SearchBlock;
  snippet: MatchSnippet;
}
export interface FindResultGroup {
  message: Message;
  count: number;
  entries: FindResultEntry[];
}
export type FindResultRow =
  | { kind: "group"; key: string; group: FindResultGroup }
  | { kind: "match"; key: string; entry: FindResultEntry };

function lowSurrogate(code: number): boolean {
  return code >= 0xdc00 && code <= 0xdfff;
}

/** Keep snippet edges on UTF-16 code-point boundaries. All text stays literal. */
export function matchSnippet(text: string, start: number, end: number, context = 60): MatchSnippet {
  let left = Math.max(0, start - context);
  let right = Math.min(text.length, end + context);
  if (left > 0 && lowSurrogate(text.charCodeAt(left))) left--;
  if (right < text.length && lowSurrogate(text.charCodeAt(right))) right++;
  return {
    before: text.slice(left, start),
    hit: text.slice(start, end),
    after: text.slice(end, right),
    leading: left > 0,
    trailing: right < text.length,
  };
}

export function groupFindResults(
  messages: readonly Message[],
  matches: readonly Match[],
  collectBlocks: (message: Message) => readonly SearchBlock[],
  newestFirst = false,
): FindResultGroup[] {
  const byOrdinal = new Map(messages.map((message) => [message.ordinal, message]));
  const groups = new Map<number, FindResultGroup>();
  const blocks = new Map<number, Map<string, SearchBlock>>();
  for (const match of matches) {
    const message = byOrdinal.get(match.ordinal);
    if (!message) continue;
    let messageBlocks = blocks.get(match.ordinal);
    if (!messageBlocks) {
      messageBlocks = new Map(collectBlocks(message).map((block) => [block.key, block]));
      blocks.set(match.ordinal, messageBlocks);
    }
    const block = messageBlocks.get(match.blockKey);
    if (!block) continue;
    let group = groups.get(match.ordinal);
    if (!group) {
      group = { message, count: 0, entries: [] };
      groups.set(match.ordinal, group);
    }
    group.count++;
    group.entries.push({ match, block, snippet: matchSnippet(block.text, match.start, match.end) });
  }
  const result = [...groups.values()].sort((a, b) => a.message.ordinal - b.message.ordinal);
  // The transcript reverses messages, not their block or character order.
  if (newestFirst) result.reverse();
  return result;
}

/** Flatten headers and occurrences so a single large message is also virtualized. */
export function resultRows(groups: readonly FindResultGroup[]): FindResultRow[] {
  return groups.flatMap((group): FindResultRow[] => [
    { kind: "group", key: `group:${group.message.ordinal}`, group },
    ...group.entries.map((entry): FindResultRow => ({
      kind: "match",
      key: `${entry.match.blockKey}:${entry.match.occurrence}`,
      entry,
    })),
  ]);
}
