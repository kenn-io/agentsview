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

  const dropdownItems = $derived.by((): FilterDropdownItem[] => {
    const mapped = items.map((item): FilterDropdownItem => {
      const included =
        mode === "include"
          ? filterSet.has(item.name)
          : !filterSet.has(item.name);
      return {
        id: item.name,
        label: item.name,
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
