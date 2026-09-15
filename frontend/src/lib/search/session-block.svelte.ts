/** Bind mounted transcript blocks to the occurrence-level session search. */
import { inSessionSearch } from "../stores/inSessionSearch.svelte.js";
import { searchBlock as attachSearchBlock } from "./search-block.svelte.js";

export function searchBlock(key: string | undefined) {
  // Index rebuilds with unchanged block state must preserve manual disclosures.
  const current = $derived(inSessionSearch.isCurrentBlock(key));
  const count = $derived(inSessionSearch.countForBlock(key));
  const occurrence = $derived(inSessionSearch.currentOccurrence(key));
  return attachSearchBlock(key, () => {
    // Re-selecting the only match is a new reveal too. It releases a manual
    // native-disclosure override without repainting every other mounted block.
    if (current) void inSessionSearch.navigationRevision;
    return {
      query: inSessionSearch.isActive ? inSessionSearch.debouncedQuery : "",
      wholeWord: inSessionSearch.wholeWord,
      count,
      current,
      occurrence,
    };
  });
}
