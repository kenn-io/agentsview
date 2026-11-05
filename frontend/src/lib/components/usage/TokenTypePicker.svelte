<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import {
    FilterDropdown,
    type FilterDropdownItem,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    ALL_TOKEN_TYPES,
    canonicalTokenTypes,
    type UsageTokenType,
  } from "../../stores/usageTokenTypes.js";

  interface Props {
    value: readonly UsageTokenType[];
    onChange: (selected: UsageTokenType[]) => void;
  }

  let { value, onChange }: Props = $props();

  function tokenTypeLabel(tokenType: UsageTokenType): string {
    switch (tokenType) {
      case "input":
        return m.usage_token_type_input();
      case "cache_write":
        return m.usage_token_type_cache_write();
      case "cache_read":
        return m.usage_token_type_cache_read();
      case "output":
        return m.usage_token_type_output();
    }
  }

  const triggerLabel = $derived.by(() => {
    if (value.length === ALL_TOKEN_TYPES.length) {
      return m.usage_token_types_all();
    }
    if (value.length === 1) {
      return m.usage_token_types_single({
        tokenType: tokenTypeLabel(value[0]!),
      });
    }
    return m.usage_token_types_multiple({
      countLabel: formatCount(value.length),
    });
  });

  const items = $derived.by((): FilterDropdownItem[] => {
    const selected = new Set(value);
    return ALL_TOKEN_TYPES.map((tokenType) => {
      const active = selected.has(tokenType);
      return {
        id: tokenType,
        label: tokenTypeLabel(tokenType),
        active,
        disabled: active && value.length === 1,
        onSelect: () => {
          const next = new Set(value);
          if (active) {
            next.delete(tokenType);
          } else {
            next.add(tokenType);
          }
          onChange(canonicalTokenTypes([...next]));
        },
      };
    });
  });
</script>

<div class="token-type-picker">
  <FilterDropdown
    label={triggerLabel}
    title={m.usage_token_types_label()}
    active={value.length < ALL_TOKEN_TYPES.length}
    showBadge={false}
    sections={[{ items }]}
    minWidth="180px"
  />
</div>

<style>
  .token-type-picker {
    display: contents;
  }

  /* Same muted chevron after the label as the other filter chips
     (usage/FilterDropdown); kit-ui FilterDropdown has none. */
  .token-type-picker :global(.kit-filter-dropdown__btn)::after {
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
