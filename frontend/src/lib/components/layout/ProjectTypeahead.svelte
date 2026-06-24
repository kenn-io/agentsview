<script lang="ts">
  import { _ } from "svelte-i18n";
  import type { ProjectInfo } from "../../api/types/core.js";
  import OptionTypeahead from "./OptionTypeahead.svelte";

  interface Props {
    projects: ProjectInfo[];
    value: string;
    onselect: (value: string) => void;
  }

  let { projects, value, onselect }: Props = $props();

  const allOption = $derived({
    name: "",
    label: $_("projectFilter.allProjects"),
    displayLabel: $_("projectFilter.allProjects"),
    count: 0,
  });

  const options = $derived.by(() => {
    const items = projects.map((p) => ({
      name: p.name,
      label: `${p.name} (${p.session_count})`,
      displayLabel: p.name,
      count: p.session_count,
    }));
    return [allOption, ...items];
  });

  const displayValue = $derived(
    value ? projects.find((p) => p.name === value)?.name ?? value : $_("projectFilter.allProjects"),
  );
</script>

<OptionTypeahead
  {options}
  {value}
  fallbackLabel={displayValue}
  placeholder={$_("projectFilter.placeholder")}
  title={$_("projectFilter.title")}
  emptyLabel={$_("projectFilter.empty")}
  {onselect}
/>
