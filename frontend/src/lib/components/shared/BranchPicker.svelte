<script lang="ts">
  import { onDestroy, tick, untrack } from "svelte";
  import {
    autoReposition,
    floatingPopoverStyle,
    SearchInput,
  } from "@kenn-io/kit-ui";
  import { CheckIcon, ChevronDownIcon } from "../../icons.js";
  import { NO_BRANCH_FILTER_TOKEN } from "../../branchFilters.js";

  export interface BranchPickerSearchParams {
    projects?: string[];
    search: string;
    limit: number;
  }

  export interface BranchPickerSearchResponse {
    branches: Array<{ branch: string }>;
    has_more: boolean;
  }

  interface Props {
    mode: "single" | "multi";
    selected: string[];
    projects: string[];
    search: (params: BranchPickerSearchParams) => Promise<BranchPickerSearchResponse>;
    label: string;
    allLabel: string;
    placeholder: string;
    clearSearchLabel: string;
    loadingLabel: string;
    emptyLabel: string;
    refineLabel: string;
    noBranchLabel?: string;
    onChange: (values: string[]) => void;
  }

  let {
    mode,
    selected,
    projects,
    search,
    label,
    allLabel,
    placeholder,
    clearSearchLabel,
    loadingLabel,
    emptyLabel,
    refineLabel,
    noBranchLabel = "",
    onChange,
  }: Props = $props();

  let open = $state(false);
  let query = $state("");
  let results: string[] = $state([]);
  let hasMore = $state(false);
  let loading = $state(false);
  let trigger: HTMLButtonElement | undefined = $state();
  let panel: HTMLDivElement | undefined = $state();
  let input: HTMLInputElement | undefined = $state();
  let panelStyle = $state("");
  let debounce: ReturnType<typeof setTimeout> | undefined;
  let requestVersion = 0;

  const normalizedSelected = $derived(
    selected.filter((value) => value !== ""),
  );
  const selectedSet = $derived(new Set(normalizedSelected));
  const rows = $derived([
    ...normalizedSelected,
    ...results.filter((branch) => !selectedSet.has(branch)).slice(0, 100),
  ]);
  const buttonLabel = $derived.by(() => {
    if (normalizedSelected.length === 0) return allLabel;
    if (normalizedSelected.length === 1) return displayBranch(normalizedSelected[0]!);
    return `${label} (${normalizedSelected.length})`;
  });

  function displayBranch(branch: string): string {
    return branch === NO_BRANCH_FILTER_TOKEN ? noBranchLabel : branch;
  }

  function positionPanel() {
    if (!trigger || !panel) return;
    const triggerRect = trigger.getBoundingClientRect();
    panelStyle = `${floatingPopoverStyle({
      trigger: triggerRect,
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: Math.min(280, window.innerWidth - 24),
      popoverHeight: panel.offsetHeight,
      triggerGap: 4,
      placement: "auto",
    })}; width: min(280px, calc(100vw - 24px))`;
  }

  async function load(searchText: string) {
    const version = ++requestVersion;
    loading = true;
    try {
      const params: BranchPickerSearchParams = {
        search: searchText,
        limit: 100,
      };
      if (projects.length > 0) params.projects = projects;
      const response = await search(params);
      if (version !== requestVersion) return;
      results = response.branches.map(({ branch }) =>
        branch === "" ? NO_BRANCH_FILTER_TOKEN : branch
      );
      hasMore = response.has_more;
    } catch {
      if (version !== requestVersion) return;
      results = [];
      hasMore = false;
    } finally {
      if (version === requestVersion) loading = false;
    }
  }

  function clearQueuedSearch() {
    if (!debounce) return;
    clearTimeout(debounce);
    debounce = undefined;
  }

  function close() {
    clearQueuedSearch();
    requestVersion++;
    loading = false;
    open = false;
  }

  function queueSearch() {
    clearQueuedSearch();
    debounce = setTimeout(() => {
      debounce = undefined;
      if (open) void load(query);
    }, 250);
  }

  async function toggleOpen() {
    if (open) {
      close();
      return;
    }
    open = true;
    query = "";
    await tick();
    positionPanel();
    input?.focus();
  }

  function choose(branch: string) {
    if (mode === "single") {
      onChange([branch]);
      close();
      trigger?.focus();
      return;
    }
    const next = selectedSet.has(branch)
      ? normalizedSelected.filter((value) => value !== branch)
      : [...normalizedSelected, branch];
    onChange(next);
  }

  function clearSelection() {
    onChange([]);
    if (mode === "single") {
      close();
      trigger?.focus();
    }
  }

  function onPanelKeydown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.preventDefault();
      close();
      trigger?.focus();
      return;
    }
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    const options = Array.from(
      panel?.querySelectorAll<HTMLButtonElement>("[role=option]") ?? [],
    );
    if (options.length === 0) return;
    event.preventDefault();
    const current = options.indexOf(document.activeElement as HTMLButtonElement);
    const delta = event.key === "ArrowDown" ? 1 : -1;
    const next = current < 0
      ? (delta > 0 ? 0 : options.length - 1)
      : (current + delta + options.length) % options.length;
    options[next]?.focus();
  }

  $effect(() => {
    const scope = projects.join("");
    if (!open) return;
    void scope;
    untrack(() => {
      clearQueuedSearch();
      requestVersion++;
      void load(query);
    });
  });

  $effect(() => {
    if (!open) return;
    void rows;
    void loading;
    void hasMore;
    return autoReposition(() => panel, positionPanel);
  });

  $effect(() => {
    if (!open) return;
    function closeOutside(event: MouseEvent) {
      const target = event.target as Node;
      if (trigger?.contains(target) || panel?.contains(target)) return;
      close();
    }
    document.addEventListener("click", closeOutside, true);
    return () => document.removeEventListener("click", closeOutside, true);
  });

  onDestroy(() => {
    clearQueuedSearch();
    requestVersion++;
  });
</script>

<div class="branch-picker">
  <button
    class="branch-picker-trigger"
    bind:this={trigger}
    type="button"
    aria-haspopup="listbox"
    aria-expanded={open}
    onclick={toggleOpen}
    title={label}
  >
    <span>{buttonLabel}</span>
    <ChevronDownIcon size="12" aria-hidden="true" />
  </button>

  {#if open}
    <div
      class="branch-picker-panel kit-popover-card"
      bind:this={panel}
      role="presentation"
      style={panelStyle}
      onkeydown={onPanelKeydown}
    >
      <div class="branch-picker-search">
        <SearchInput
          bind:inputEl={input}
          bind:value={query}
          size="sm"
          block
          {placeholder}
          ariaLabel={placeholder}
          clearLabel={clearSearchLabel}
          oninput={() => queueSearch()}
        />
      </div>
      <!-- kit-ui-check-ignore: kit-ui has no remote multi-select with pinned selected rows. -->
      <div class="branch-picker-list" role="listbox" aria-multiselectable={mode === "multi"}>
        <button
          class="branch-picker-row all"
          class:selected={normalizedSelected.length === 0}
          type="button"
          role="option"
          aria-selected={normalizedSelected.length === 0}
          onclick={clearSelection}
        >
          <span class="branch-picker-check">
            {#if normalizedSelected.length === 0}
              <CheckIcon size="9" strokeWidth="2.4" aria-hidden="true" />
            {/if}
          </span>
          <span>{allLabel}</span>
        </button>
        {#each rows as branch (branch)}
          <button
            class="branch-picker-row"
            class:selected={selectedSet.has(branch)}
            type="button"
            role="option"
            aria-selected={selectedSet.has(branch)}
            onclick={() => choose(branch)}
          >
            <span class="branch-picker-check">
              {#if selectedSet.has(branch)}
                <CheckIcon size="9" strokeWidth="2.4" aria-hidden="true" />
              {/if}
            </span>
            <span class="branch-picker-name">{displayBranch(branch)}</span>
          </button>
        {/each}
        {#if loading}
          <div class="branch-picker-status" role="status">{loadingLabel}</div>
        {:else if rows.length === 0}
          <div class="branch-picker-status">{emptyLabel}</div>
        {/if}
        {#if !loading && hasMore}
          <div class="branch-picker-refine">{refineLabel}</div>
        {/if}
      </div>
    </div>
  {/if}
</div>

<style>
  .branch-picker {
    position: relative;
  }

  .branch-picker-trigger {
    min-width: var(--branch-picker-min-width, 132px);
    max-width: var(--branch-picker-max-width, 184px);
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 0 8px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: 11px;
  }

  .branch-picker-trigger span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .branch-picker-panel {
    position: fixed;
    z-index: var(--z-popover);
    padding: 8px;
  }

  .branch-picker-search {
    margin-bottom: var(--space-3);
  }

  .branch-picker-list {
    max-height: min(360px, calc(100vh - 128px));
    overflow-y: auto;
    overscroll-behavior: contain;
  }

  .branch-picker-row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: 5px 7px;
    color: var(--text-secondary);
    font-size: 11px;
    text-align: left;
    border-radius: var(--radius-sm);
  }

  .branch-picker-row:hover,
  .branch-picker-row:focus-visible {
    background: var(--bg-surface-hover);
    outline: none;
  }

  .branch-picker-row.selected {
    color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
  }

  .branch-picker-check {
    width: 12px;
    height: 12px;
    flex: 0 0 12px;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .branch-picker-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .branch-picker-status,
  .branch-picker-refine {
    padding: 8px;
    color: var(--text-muted);
    font-size: 10px;
    text-align: center;
  }

  .branch-picker-refine {
    border-top: 1px solid var(--border-muted);
  }
</style>
