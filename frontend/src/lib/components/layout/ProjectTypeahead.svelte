<script lang="ts">
  import { Typeahead } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import type { ProjectInfo } from "../../api/types/core.js";

  interface Props {
    projects: ProjectInfo[];
    value: string;
    onselect: (value: string) => void;
  }

  let { projects, value, onselect }: Props = $props();

  const allOption = {
    name: "",
    label: m.shared_all_projects(),
    displayLabel: m.shared_all_projects(),
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
    value ? projects.find((p) => p.name === value)?.name ?? value : m.shared_all_projects(),
  );
</script>

<Typeahead
  {options}
  {value}
  fallbackLabel={displayValue}
  placeholder={m.shared_project_filter_placeholder()}
  title={m.shared_select_project()}
  emptyLabel={m.shared_no_matching_projects()}
  {onselect}
/>
