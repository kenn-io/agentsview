<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import { Typeahead } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import type { DbProjectInfo as ProjectInfo } from "../../api/generated/index.js";

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
    disabled?: boolean;
    /** Dim label before the value on the closed trigger ("Project:"). */
    triggerPrefix?: string;
    /** Closed-trigger text for the all-projects choice; defaults to the
     * full "All Projects" label. Filter chips pass a short "All". */
    allLabel?: string;
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
    disabled = false,
    triggerPrefix = "",
    allLabel = m.shared_all_projects(),
  }: Props = $props();

  const allOption = $derived({
    name: "",
    label: m.shared_all_projects(),
    displayLabel: allLabel,
    count: 0,
  });

  const options = $derived.by(() => {
    // An empty-name project would duplicate the all-projects key, and "" already means unfiltered.
    const items = projects
      .filter((p) => p.name !== "")
      .map((p) => ({
        name: p.name,
        label: p.name,
        displayLabel: p.name,
        // The count sits in the trailing meta lane, so a long name
        // truncates before its count instead of pushing it out of view.
        meta: formatCount(p.session_count),
        count: p.session_count,
      }));
    return includeAll ? [allOption, ...items] : items;
  });

  const displayValue = $derived(
    value
      ? projects.find((p) => p.name === value)?.name ?? value
      : includeAll
        ? allLabel
        : placeholder,
  );
</script>

<Typeahead
  {disabled}
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
  {triggerPrefix}
/>
