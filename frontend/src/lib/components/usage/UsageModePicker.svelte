<script lang="ts">
  import {
    SegmentedControl,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import type { UsageMode } from "../../stores/usage.svelte.js";

  interface Props {
    value: UsageMode;
    onChange: (mode: UsageMode) => void;
  }

  let { value, onChange }: Props = $props();

  const options = $derived<SegmentedControlOption[]>([
    { value: "cost", label: m.usage_mode_cost() },
    { value: "token", label: m.usage_mode_tokens() },
  ]);
</script>

<SegmentedControl
  {options}
  {value}
  ariaLabel={m.usage_mode_label()}
  onchange={(next) => onChange(next as UsageMode)}
/>
