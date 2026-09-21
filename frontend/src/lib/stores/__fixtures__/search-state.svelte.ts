/** Reactive dependencies used by the local search store tests. */
import type { SearchMessageSource, SearchView } from "../inSessionSearch.svelte.js";

type SourceFixture = Omit<SearchMessageSource, "historyComplete" | "ensureHistoryLoaded"> &
  Partial<Pick<SearchMessageSource, "historyComplete" | "ensureHistoryLoaded">>;

export function reactiveSource(initial: SourceFixture): SearchMessageSource {
  const source: SearchMessageSource = $state({
    ...initial,
    historyComplete: initial.historyComplete ?? true,
    ensureHistoryLoaded: initial.ensureHistoryLoaded ?? (() => source.ensureOrdinalLoaded(0)),
  });
  return source;
}

export function reactiveView(initial: SearchView): SearchView {
  const view = $state(initial);
  return view;
}
