<script lang="ts">
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
      countLabel: String(value.length),
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

<FilterDropdown
  label={triggerLabel}
  title={m.usage_token_types_label()}
  active={value.length < ALL_TOKEN_TYPES.length}
  showBadge={false}
  sections={[{ items }]}
  minWidth="180px"
/>
