<script lang="ts">
  import { Button, IconButton } from "@kenn-io/kit-ui";
  import { XIcon } from "../../icons.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { DbProjectInventoryRow } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectReclassificationEditor from "./ProjectReclassificationEditor.svelte";

  interface Props {
    row: DbProjectInventoryRow;
    projects: ProjectInfo[];
    readOnly: boolean;
    onClose: () => void;
    onRefresh: (projectKey: string, appliedTarget: string) => Promise<boolean>;
    onComplete: () => void;
    onOpenRules: (machine: string) => void;
  }

  let { row, projects, readOnly, onClose, onRefresh, onComplete, onOpenRules }: Props =
    $props();

  // Hosts remount this component whenever the selected project changes (see
  // the editor's remount contract), so a mount-time snapshot identifies this
  // workspace for its whole lifetime. The editor deliberately still refreshes
  // after a mid-apply unmount; passing the snapshot instead of the host's
  // live selection lets the host tell a rename apart from a dismissal.
  // svelte-ignore state_referenced_locally
  const workspaceKey = row.project_key;

  // Display-only fallback for empty labels (absolute-path/URL-scheme
  // projects). The editor below must still receive the raw label, since it
  // feeds original_project and API calls.
  const displayLabel = $derived(row.label || m.shared_unknown());

  function fmtDate(value: string | null | undefined): string {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "—";
    return formatDateTime(date, { year: "numeric", month: "short", day: "numeric" });
  }

  function onkeydown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
    }
  }
</script>

<!-- The Escape handler is a scoped keyboard shortcut for the panel, not the
     panel's primary interaction; the close button remains the accessible
     control. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<section class="workspace" aria-label={displayLabel} {onkeydown}>
  <header class="workspace-header">
    <span class="back-btn">
      <Button size="sm" label={m.data_workspace_all_projects()} onclick={onClose} />
    </span>
    <h3>{displayLabel}</h3>
    <IconButton size="sm" ariaLabel={m.data_workspace_close()} onclick={onClose}>
      <XIcon size="14" aria-hidden="true" />
    </IconButton>
  </header>
  <dl class="context">
    <div><dt>{m.data_col_sessions()}</dt><dd>{row.sessions}</dd></div>
    <div><dt>{m.data_col_machines()}</dt><dd>{row.machines}</dd></div>
    <div><dt>{m.data_col_cwds()}</dt><dd>{row.distinct_cwds}</dd></div>
    <div>
      <dt>{m.data_workspace_activity()}</dt>
      <dd>
        {#if row.first_activity || row.last_activity}
          {m.data_workspace_activity_range({
            first: fmtDate(row.first_activity),
            last: fmtDate(row.last_activity),
          })}
        {:else}
          {m.data_workspace_no_activity()}
        {/if}
      </dd>
    </div>
  </dl>
  {#if row.enabled_rules_targeting > 0 || row.recorded_as_original}
    <p class="annotations">
      {#if row.enabled_rules_targeting > 0}
        <span>{m.data_rules_targeting({ count: row.enabled_rules_targeting })}</span>
      {/if}
      {#if row.recorded_as_original}
        <span>{m.data_recorded_original()}</span>
      {/if}
    </p>
  {/if}
  <ProjectReclassificationEditor
    projectLabel={row.label}
    projectKey={row.project_key}
    {projects}
    {readOnly}
    onRefresh={(target) => onRefresh(workspaceKey, target)}
    {onComplete}
    {onOpenRules}
  />
</section>

<style>
  .workspace {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .workspace-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  h3 {
    flex: 1;
    margin: 0;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .back-btn {
    display: none;
  }

  .context {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
    gap: 8px;
    margin: 0;
  }

  .context dt {
    color: var(--text-muted);
    font-size: 11px;
  }

  .context dd {
    margin: 0;
    font-size: 12px;
    color: var(--text-secondary);
  }

  .annotations {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
    margin: 0;
    color: var(--text-muted);
    font-size: 11px;
  }

  @media (max-width: 760px) {
    .back-btn {
      display: inline-flex;
    }
  }
</style>
