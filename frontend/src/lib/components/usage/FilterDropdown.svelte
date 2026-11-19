<!-- Thin app-glue wrapper around kit-ui's FilterDropdown. It keeps the
     usage/analytics domain API (CSV-encoded exclusion/inclusion sets and
     the localized trigger label) and maps it onto kit-ui's sectioned
     item model; all popover markup and behavior live in kit-ui. -->
<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import {
    FilterDropdown,
    type FilterDropdownItem,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";

  interface FilterItem {
    id?: string;
    name: string;
    count?: number;
    description?: string;
  }

  interface Props {
    label: string;
    selectedLabel?: string;
    items: FilterItem[];
    /** Comma-separated list of excluded item IDs. */
    excludedCsv: string;
    onToggle: (id: string) => void;
    onSelectAll?: () => void;
    onDeselectAll?: () => void;
    color?: (name: string) => string;
    mode?: "exclude" | "include";
    /** Active exclusions represented outside excludedCsv. */
    unlistedExcludedCount?: number;
  }

  let {
    label,
    selectedLabel = "",
    items,
    excludedCsv,
    onToggle,
    onSelectAll,
    onDeselectAll,
    color,
    mode = "exclude",
    unlistedExcludedCount = 0,
  }: Props = $props();

  function itemId(item: FilterItem): string {
    return item.id ?? item.name;
  }

  const filterSet = $derived(
    new Set(excludedCsv ? excludedCsv.split(",") : []),
  );

  const listedFilteredCount = $derived(
    items.reduce(
      (count, item) => count + (filterSet.has(itemId(item)) ? 1 : 0),
      0,
    ),
  );
  const filteredCount = $derived(
    filterSet.size + Math.max(0, unlistedExcludedCount),
  );
  const unlistedFilteredCount = $derived(
    Math.max(0, filterSet.size - listedFilteredCount) +
      Math.max(0, unlistedExcludedCount),
  );
  const visibleCount = $derived(
    items.length - listedFilteredCount,
  );

  const buttonLabel = $derived.by(() => {
    if (selectedLabel) return `${label}: ${selectedLabel}`;
    if (filteredCount === 0) return m.usage_filter_all({ label });
    if (mode === "include") {
      if (filteredCount === 1) return `${label}: ${excludedCsv}`;
      return m.usage_filter_selected({
        label,
        countLabel: formatCount(filteredCount),
      });
    }
    if (visibleCount === 1 && unlistedFilteredCount === 0) {
      const visible = items.find(
        (item) => !filterSet.has(itemId(item)),
      );
      if (visible) {
        const maxLen = 20;
        if (visible.name.length > maxLen) {
          return `${label}: ${visible.name.slice(0, maxLen)}...`;
        }
        return `${label}: ${visible.name}`;
      }
    }
    if (visibleCount === 0 && unlistedFilteredCount === 0) {
      return m.usage_filter_none({ label });
    }
    return m.usage_filter_hidden({
      label,
      countLabel: formatCount(filteredCount),
    });
  });

  const dropdownItems = $derived.by((): FilterDropdownItem[] => {
    const mapped = items.map((item): FilterDropdownItem => {
      const id = itemId(item);
      const included =
        mode === "include"
          ? filterSet.has(id)
          : !filterSet.has(id);
      return {
        id,
        label: item.name,
        description: item.description,
        active: included,
        count: item.count,
        color: color?.(item.name),
        onSelect: () => onToggle(id),
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

<div class="filter-chip-dropdown">
<FilterDropdown
  label={buttonLabel}
  active={filteredCount > 0 || selectedLabel !== ""}
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
  /* Same chip height as the Typeahead filter chips (shared/FilterChip). */
  .filter-chip-dropdown {
    display: contents;
    --kit-control-height: var(--filter-chip-height);
  }

  /* Like the Typeahead chips, these carry no funnel: the "Project:" label
     names the filter, and the page's single filter button keeps the
     funnel. kit-ui FilterDropdown has no icon-less variant yet. */
  .filter-chip-dropdown :global(.kit-filter-dropdown__btn > svg.lucide-funnel) {
    display: none;
  }

  /* kit-ui FilterDropdown has no chevron; draw the Typeahead chips' 12px
     muted chevron after the label so every filter chip that opens a list
     shows it. Kept in step with usage/TokenTypePicker. */
  .filter-chip-dropdown :global(.kit-filter-dropdown__btn)::after {
    content: "";
    flex-shrink: 0;
    width: 12px;
    height: 12px;
    margin-inline-start: -1px;
    background: var(--text-muted);
    opacity: 0.8;
    -webkit-mask: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='black' stroke-width='2.25' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='m6 9 6 6 6-6'/%3E%3C/svg%3E") center / 12px 12px no-repeat;
    mask: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='black' stroke-width='2.25' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='m6 9 6 6 6-6'/%3E%3C/svg%3E") center / 12px 12px no-repeat;
  }
</style>
