<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { CheckIcon, ChevronDownIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  interface FilterItem {
    name: string;
    count?: number;
  }

  interface Props {
    label: string;
    items: FilterItem[];
    /** Comma-separated list of EXCLUDED item names. */
    excludedCsv: string;
    onToggle: (name: string) => void;
    onSelectAll?: () => void;
    onDeselectAll?: () => void;
    color?: (name: string) => string;
    mode?: "exclude" | "include";
  }

  let {
    label,
    items,
    excludedCsv,
    onToggle,
    onSelectAll,
    onDeselectAll,
    color,
    mode = "exclude",
  }: Props = $props();

  let open = $state(false);
  let search = $state("");
  let containerEl: HTMLDivElement | undefined = $state();

  const filterSet = $derived(
    new Set(excludedCsv ? excludedCsv.split(",") : []),
  );

  const filteredCount = $derived(filterSet.size);
  const visibleCount = $derived(
    items.length - filteredCount,
  );

  const buttonLabel = $derived.by(() => {
    if (filteredCount === 0) return m.usage_filter_all({ label });
    if (mode === "include") {
      if (filteredCount === 1) return `${label}: ${excludedCsv}`;
      return m.usage_filter_selected({
        label,
        countLabel: filteredCount.toLocaleString(),
      });
    }
    if (visibleCount === 1) {
      const visible = items.find(
        (i) => !filterSet.has(i.name),
      );
      if (visible) {
        const maxLen = 20;
        if (visible.name.length > maxLen) {
          return `${label}: ${visible.name.slice(0, maxLen)}...`;
        }
        return `${label}: ${visible.name}`;
      }
    }
    if (visibleCount === 0) return m.usage_filter_none({ label });
    return m.usage_filter_hidden({
      label,
      countLabel: filteredCount.toLocaleString(),
    });
  });

  const showSearch = $derived(items.length > 8);

  const filtered = $derived(
    search
      ? items.filter((i) =>
          i.name.toLowerCase().includes(
            search.toLowerCase(),
          ),
        )
      : items,
  );

  function handleClickOutside(e: MouseEvent) {
    if (
      containerEl &&
      !containerEl.contains(e.target as Node)
    ) {
      open = false;
      search = "";
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape" && open) {
      open = false;
      search = "";
    }
  }

  onMount(() => {
    document.addEventListener("click", handleClickOutside);
    document.addEventListener("keydown", handleKeydown);
  });

  onDestroy(() => {
    document.removeEventListener(
      "click",
      handleClickOutside,
    );
    document.removeEventListener("keydown", handleKeydown);
  });
</script>

<div class="filter-dropdown" bind:this={containerEl}>
  <button
    class="filter-trigger"
    class:active={filteredCount > 0}
    onclick={() => {
      open = !open;
      if (!open) search = "";
    }}
  >
    <span class="trigger-label">{buttonLabel}</span>
    <ChevronDownIcon class={open ? "chevron open" : "chevron"} size="10" strokeWidth="2.2" aria-hidden="true" />
  </button>

  {#if open}
    <div class="dropdown-panel">
      {#if showSearch}
        <input
          class="dropdown-search"
          type="text"
          placeholder={m.usage_filter_search()}
          bind:value={search}
        />
      {/if}
      {#if mode === "exclude"}
        <div class="bulk-actions">
          <button
            class="bulk-btn"
            onclick={() => onSelectAll?.()}
          >{m.usage_filter_select_all()}</button>
          <button
            class="bulk-btn"
            onclick={() => onDeselectAll?.()}
          >{m.usage_filter_deselect_all()}</button>
        </div>
      {/if}
      <div class="dropdown-list">
        {#if mode === "include"}
          <button
            class="dropdown-row"
            class:selected={filteredCount === 0}
            style:--item-color={"var(--accent-blue)"}
            onclick={() => onSelectAll?.()}
          >
            <span
              class="item-check"
              class:on={filteredCount === 0}
            >
              {#if filteredCount === 0}
                <CheckIcon size="8" strokeWidth="2.4" aria-hidden="true" />
              {/if}
            </span>
            <span class="item-name">
              {m.usage_filter_all_items({
                label: label.toLowerCase(),
                count: items.length,
              })}
            </span>
          </button>
        {/if}
        {#each filtered as item (item.name)}
          {@const included = mode === "include"
            ? filterSet.has(item.name)
            : !filterSet.has(item.name)}
          <button
            class="dropdown-row"
            class:selected={included}
            style:--item-color={color
              ? color(item.name)
              : "var(--accent-blue)"}
            onclick={() => onToggle(item.name)}
          >
            <span
              class="item-check"
              class:on={included}
            >
              {#if included}
                <CheckIcon size="8" strokeWidth="2.4" aria-hidden="true" />
              {/if}
            </span>
            {#if color}
              <span
                class="color-dot"
                style:background={color(item.name)}
              ></span>
            {/if}
            <span class="item-name">{item.name}</span>
            {#if item.count !== undefined}
              <span class="item-count">{item.count}</span>
            {/if}
          </button>
        {/each}
        {#if filtered.length === 0}
          <div class="dropdown-empty">{m.sidebar_filters_no_match()}</div>
        {/if}
      </div>
    </div>
  {/if}
</div>

<style>
  .filter-dropdown {
    position: relative;
  }

  .filter-trigger {
    height: 26px;
    padding: 0 8px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-size: 11px;
    display: flex;
    align-items: center;
    gap: 4px;
    cursor: pointer;
    white-space: nowrap;
    max-width: 200px;
  }

  .filter-trigger:hover {
    background: var(--bg-surface-hover);
  }

  .filter-trigger.active {
    border-color: var(--accent-blue);
    background: color-mix(
      in srgb,
      var(--accent-blue) 8%,
      var(--bg-inset)
    );
  }

  .trigger-label {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  :global(.chevron) {
    flex-shrink: 0;
    transition: transform 0.15s;
  }

  :global(.chevron.open) {
    transform: rotate(180deg);
  }

  .dropdown-panel {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    z-index: 100;
    min-width: 180px;
    max-width: 280px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
    overflow: hidden;
  }

  .dropdown-search {
    width: 100%;
    padding: 6px 8px;
    border: none;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-size: 11px;
    outline: none;
    box-sizing: border-box;
  }

  .dropdown-search::placeholder {
    color: var(--text-muted);
  }

  .bulk-actions {
    display: flex;
    gap: 4px;
    padding: 4px 8px;
    border-bottom: 1px solid var(--border-muted);
  }

  .bulk-btn {
    font-size: 10px;
    color: var(--accent-blue);
    cursor: pointer;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
  }

  .bulk-btn:hover {
    background: var(--bg-surface-hover);
  }

  .dropdown-list {
    max-height: 240px;
    overflow-y: auto;
    padding: 2px 0;
  }

  .dropdown-row {
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    padding: 4px 8px;
    border: none;
    background: none;
    color: var(--text-primary);
    font-size: 11px;
    cursor: pointer;
    text-align: left;
    flex-shrink: 0;
  }

  .dropdown-row:hover {
    background: var(--bg-surface-hover);
  }

  .dropdown-row.selected {
    color: var(--item-color, var(--accent-blue));
    font-weight: 500;
    background: color-mix(
      in srgb,
      var(--item-color, var(--accent-blue)) 8%,
      transparent
    );
  }

  .item-check {
    width: 10px;
    height: 10px;
    border-radius: 2px;
    border: 1px solid var(--border-muted);
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .item-check.on {
    background: var(--item-color, var(--accent-blue));
    border-color: var(--item-color, var(--accent-blue));
  }

  .color-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .item-name {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .item-count {
    color: var(--text-muted);
    font-size: 10px;
    flex-shrink: 0;
  }

  .dropdown-empty {
    padding: 8px;
    text-align: center;
    color: var(--text-muted);
    font-size: 11px;
  }
</style>
