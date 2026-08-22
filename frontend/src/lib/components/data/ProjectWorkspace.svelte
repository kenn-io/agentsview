<script lang="ts">
  import { Button, Chip, IconButton } from "@kenn-io/kit-ui";
  import { XIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import type { DbProjectInventoryRow } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectBatchReclassificationEditor from "./ProjectBatchReclassificationEditor.svelte";
  import ProjectReclassificationEditor from "./ProjectReclassificationEditor.svelte";
  import ProjectSessionPreviewPane from "./ProjectSessionPreviewPane.svelte";
  import { displayProjectLabel } from "./project-label.js";

  interface Props {
    row: DbProjectInventoryRow;
    projects: ProjectInfo[];
    readOnly: boolean;
    onClose: () => void;
    onRefresh: (projectKey: string, appliedTarget: string) => Promise<boolean>;
    onComplete: (target: string) => void;
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

  // Display-only copy distinguishes empty private labels and the parser's
  // "unknown" sentinel. The editor below must still receive the raw label,
  // since it feeds original_project and API calls.
  const displayLabel = $derived(displayProjectLabel(row.label));
  const batchRows = $derived([row]);
  let suggestionCount = $state(0);
  let wholeProjectMode = $state(false);

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
    <div class="workspace-title">
      <div class="title-line">
        <h3>{displayLabel}</h3>
        {#if row.enabled_rules_targeting > 0}
          <Chip size="xs" tone="info" uppercase={false}>
            {m.data_rules_targeting({ count: row.enabled_rules_targeting })}
          </Chip>
        {/if}
      </div>
      <div class="project-facts">
        <span>{m.data_summary_sessions({ count: row.sessions })}</span>
        <span>{m.data_summary_machines({ count: row.machines })}</span>
        <span>{m.data_summary_folder_suggestions({ count: suggestionCount })}</span>
      </div>
    </div>
    <div class="workspace-actions">
      <Button
        size="sm"
        label={wholeProjectMode
          ? m.data_workspace_correct_one_folder()
          : m.data_workspace_map_whole_project()}
        onclick={() => (wholeProjectMode = !wholeProjectMode)}
      />
      <IconButton size="sm" ariaLabel={m.data_workspace_close()} onclick={onClose}>
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>
  </header>
  <div class="correction-pane">
    {#if wholeProjectMode}
      <ProjectBatchReclassificationEditor
        rows={batchRows}
        {projects}
        {readOnly}
        onRefresh={(target) => onRefresh(workspaceKey, target)}
        onComplete={(target) => onComplete(target)}
        {onOpenRules}
        onCandidateCount={(count) => (suggestionCount = count)}
      />
    {:else}
      <ProjectReclassificationEditor
        projectLabel={row.label}
        projectKey={row.project_key}
        {projects}
        {readOnly}
        onRefresh={(target) => onRefresh(workspaceKey, target)}
        {onComplete}
        {onOpenRules}
        onCandidateCount={(count) => (suggestionCount = count)}
      />
    {/if}
  </div>
  <div class="project-session-pane">
    <ProjectSessionPreviewPane projectLabel={row.label} />
  </div>
</section>

<style>
  .workspace {
    display: flex;
    height: 100%;
    flex-direction: column;
    min-height: 0;
  }

  .workspace-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 11px 12px;
    border-bottom: 1px solid var(--border-muted);
  }

  .workspace-title {
    flex: 1;
    min-width: 0;
  }

  .workspace-actions {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  .title-line {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  h3 {
    margin: 0;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .back-btn {
    display: none;
  }

  .project-facts {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-6);
    margin-top: 7px;
    color: var(--text-muted);
    font-size: 10px;
  }

  .correction-pane {
    display: flex;
    flex: 0 1 auto;
    min-height: 0;
    overflow: hidden;
  }

  .project-session-pane {
    flex: 1 0 180px;
    min-height: 180px;
    overflow-y: auto;
    padding: 10px 12px;
    border-top: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }

  @media (max-width: 760px) {
    .back-btn {
      display: inline-flex;
    }
  }
</style>
