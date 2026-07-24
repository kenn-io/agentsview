<!-- Thin app-glue wrapper around kit-ui's FilterDropdown. It keeps the
     usage/analytics domain API (CSV-encoded exclusion/inclusion sets and
     the localized trigger label) and maps it onto kit-ui's sectioned
     item model; all popover markup and behavior live in kit-ui. -->
<script lang="ts">
  import {
    FilterDropdown,
    type FilterDropdownItem,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";

  interface FilterItem {
    name: string;
    /** Display label when name is an opaque identity (e.g. branch tokens). */
    label?: string;
    count?: number;
  }

  interface Props {
    label: string;
    items: FilterItem[];
    /** Separator-joined list of EXCLUDED item names. */
    excludedCsv: string;
    /** List separator; branch tokens can contain commas. */
    separator?: string;
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
    separator = ",",
    onToggle,
    onSelectAll,
    onDeselectAll,
    color,
    mode = "exclude",
  }: Props = $props();

  const filterSet = $derived(
    new Set(excludedCsv ? excludedCsv.split(separator) : []),
  );

  const filteredCount = $derived(filterSet.size);
  const visibleCount = $derived(
    items.length - filteredCount,
  );

  function singleItemLabel(item: FilterItem): string {
    const display = item.label ?? item.name;
    const maxLen = 20;
    if (display.length > maxLen) {
      return `${label}: ${display.slice(0, maxLen)}...`;
    }
    return `${label}: ${display}`;
  }

  const buttonLabel = $derived.by(() => {
    if (filteredCount === 0) return m.usage_filter_all({ label });
    if (mode === "include") {
      if (filteredCount === 1) {
        // Resolve the display label: the name can be an opaque token
        // (branch tokens embed a control-character separator).
        const selected = items.find((i) => filterSet.has(i.name));
        if (selected) return singleItemLabel(selected);
      }
      return m.usage_filter_selected({
        label,
        countLabel: filteredCount.toLocaleString(),
      });
    }
    if (visibleCount === 1) {
      const visible = items.find(
        (i) => !filterSet.has(i.name),
      );
      if (visible) return singleItemLabel(visible);
    }
    if (visibleCount === 0) return m.usage_filter_none({ label });
    return m.usage_filter_hidden({
      label,
      countLabel: filteredCount.toLocaleString(),
    });
  });

  const dropdownItems = $derived.by((): FilterDropdownItem[] => {
    const mapped = items.map((item): FilterDropdownItem => {
      const included =
        mode === "include"
          ? filterSet.has(item.name)
          : !filterSet.has(item.name);
      return {
        id: item.name,
        label: item.label ?? item.name,
        active: included,
        count: item.count,
        color: color?.(item.name),
        onSelect: () => onToggle(item.name),
      };
    });
    if (mode === "include") {
      // "All <items>" row: selecting it clears the inclusion filter.
      mapped.unshift({
        id: "__all__",
        label: m.usage_filter_all_items({
          label: label.toLowerCase(),
          count: items.length,
        }),
        active: filteredCount === 0,
        color: "var(--accent-blue)",
        onSelect: () => onSelectAll?.(),
      });
    }
    return mapped;
  });
</script>

<div class="filter-dropdown-clamp">
  <FilterDropdown
    label={buttonLabel}
    active={filteredCount > 0}
    showBadge={false}
    sections={[{ items: dropdownItems }]}
    searchable={items.length > 8}
    searchPlaceholder={m.usage_filter_search()}
    emptyLabel={m.sidebar_filters_no_match()}
    onSelectAll={mode === "exclude" ? onSelectAll : undefined}
    onDeselectAll={mode === "exclude" ? onDeselectAll : undefined}
    selectAllLabel={m.usage_filter_select_all()}
    deselectAllLabel={m.usage_filter_deselect_all()}
  />
</div>

<style>
  .filter-dropdown-clamp {
    display: contents;
  }

  /* kit-ui's panel has no height cap, so an uncapped item list (the
     branch dropdown can run to thousands of entries) grows past the
     viewport bottom with no way to scroll. Clamp and scroll here until
     the cap lands upstream in kit-ui. */
  .filter-dropdown-clamp :global(.kit-filter-dropdown__panel) {
    max-height: min(480px, calc(100vh - 96px));
    overflow-y: auto;
    overscroll-behavior: contain;
  }
</style>
