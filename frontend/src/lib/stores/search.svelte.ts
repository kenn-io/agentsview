import { SearchService } from "../api/generated/index";
import {
  configureGeneratedClient,
  isAbortError,
  withAbort,
} from "../api/runtime.js";
import { debounce } from "@kenn-io/kit-ui";
import type {
  SearchResponse,
  SearchResult,
} from "../api/types.js";

class SearchStore {
  query: string = $state("");
  project: string = $state("");
  sort: "relevance" | "recency" = $state("relevance");
  results: SearchResult[] = $state([]);
  isSearching: boolean = $state(false);

  private abortController: AbortController | null = null;

  private debouncedSearch = debounce(
    (q: string, project: string) => {
      this.executeSearch(q, project);
    },
    300,
  );

  search(q: string, project?: string) {
    this.query = q;
    if (project !== undefined) this.project = project;

    if (!q.trim()) {
      this.debouncedSearch.cancel();
      this.abortController?.abort();
      this.results = [];
      this.isSearching = false;
      return;
    }

    this.abortController?.abort();
    this.abortController = null;
    this.debouncedSearch(q, this.project);
  }

  setSort(s: "relevance" | "recency") {
    this.sort = s;
    if (this.query.trim()) {
      this.debouncedSearch.cancel();
      this.executeSearch(this.query, this.project);
    }
  }

  clear() {
    this.query = "";
    this.results = [];
    this.isSearching = false;
    this.debouncedSearch.cancel();
    this.abortController?.abort();
  }

  /** Full reset: clears results and resets sort to the default.
   * Call this on palette close, not on transient clears (e.g. query < 3 chars). */
  resetSort() {
    this.sort = "relevance";
  }

  private async executeSearch(
    q: string, project: string,
  ) {
    this.abortController?.abort();
    this.abortController = new AbortController();
    const { signal } = this.abortController;

    this.isSearching = true;
    try {
      configureGeneratedClient();
      const res = await withAbort(
        SearchService.getApiV1Search({
          q,
          project: project || undefined,
          limit: 30,
          sort: this.sort,
        }) as unknown as Promise<SearchResponse>,
        signal,
      );
      this.results = res.results ?? [];
    } catch (error: unknown) {
      if (isAbortError(error)) {
        return;
      }
      this.results = [];
    } finally {
      if (!signal.aborted) {
        this.isSearching = false;
      }
    }
  }
}

export const searchStore = new SearchStore();
