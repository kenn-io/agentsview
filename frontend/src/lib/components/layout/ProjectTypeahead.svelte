<script lang="ts">
  import { Typeahead } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import type { ProjectInfo } from "../../api/types/core.js";

  interface Props {
    projects: ProjectInfo[];
    value: string;
    onselect: (value: string) => void;
    onquery?: (query: string) => void;
    includeAll?: boolean;
    allowCustom?: boolean;
    customLabel?: string;
    placeholder?: string;
    title?: string;
    emptyLabel?: string;
  }

  let {
    projects,
    value,
    onselect,
    onquery = undefined,
    includeAll = true,
    allowCustom = false,
    customLabel = m.data_reclassify_use_custom_project({ query: "{query}" }),
    placeholder = m.shared_project_filter_placeholder(),
    title = m.shared_select_project(),
    emptyLabel = m.shared_no_matching_projects(),
  }: Props = $props();

  const allOption = {
    name: "",
    label: m.shared_all_projects(),
    displayLabel: m.shared_all_projects(),
    count: 0,
  };

  const options = $derived.by(() => {
    // An empty-name project would duplicate the all-projects key, and "" already means unfiltered.
    const items = projects
      .filter((p) => p.name !== "")
      .map((p) => ({
        name: p.name,
        label: `${p.name} (${p.session_count})`,
        displayLabel: p.name,
        count: p.session_count,
      }));
    return includeAll ? [allOption, ...items] : items;
  });

  const displayValue = $derived(
    value
      ? projects.find((p) => p.name === value)?.name ?? value
      : includeAll
        ? m.shared_all_projects()
        : placeholder,
  );
</script>

<Typeahead
  {options}
  {value}
  fallbackLabel={displayValue}
  {placeholder}
  {title}
  {emptyLabel}
  {allowCustom}
  {customLabel}
  {onquery}
  {onselect}
/>
