<script lang="ts">
  import type { ProjectInfo } from "../../api/types/core.js";
  import OptionTypeahead from "./OptionTypeahead.svelte";

  interface Props {
    projects: ProjectInfo[];
    value: string;
    onselect: (value: string) => void;
  }

  let { projects, value, onselect }: Props = $props();

  const allOption = {
    name: "",
    label: "All Projects",
    displayLabel: "All Projects",
    count: 0,
  };

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
    value ? projects.find((p) => p.name === value)?.name ?? value : "All Projects",
  );
</script>

<OptionTypeahead
  {options}
  {value}
  fallbackLabel={displayValue}
  placeholder="Filter projects..."
  title="Select project"
  emptyLabel="No matching projects"
  {onselect}
/>
