<script lang="ts">
  import { Chip, IconButton } from "@kenn-io/kit-ui";
  import { XIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import type { DbProjectInventoryRow } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectBatchReclassificationEditor from "./ProjectBatchReclassificationEditor.svelte";

  interface Props {
    rows: DbProjectInventoryRow[];
    projects: ProjectInfo[];
    readOnly: boolean;
    onClose: () => void;
    onRefresh: (target: string) => Promise<boolean>;
    onComplete: (target: string, count: number) => void;
    onOpenRules: (machine: string) => void;
  }

  let { rows, projects, readOnly, onClose, onRefresh, onComplete, onOpenRules }: Props = $props();
  let suggestionCount = $state(0);

  const sessionCount = $derived(rows.reduce((sum, row) => sum + row.sessions, 0));

  function onkeydown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.stopPropagation();
      onClose();
    }
  }
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<section class="workspace" aria-label={m.data_batch_workspace_label()} {onkeydown}>
  <header class="workspace-header">
    <div class="workspace-title">
      <div class="title-line">
        <h3>{m.data_batch_heading({ count: rows.length })}</h3>
        <Chip size="xs" tone="info" uppercase={false}>{m.data_batch_label()}</Chip>
      </div>
      <div class="project-facts">
        <span>{m.data_summary_sessions({ count: sessionCount })}</span>
        <span>{m.data_summary_folder_suggestions({ count: suggestionCount })}</span>
      </div>
    </div>
    <IconButton size="sm" ariaLabel={m.data_workspace_close()} onclick={onClose}>
      <XIcon size="14" aria-hidden="true" />
    </IconButton>
  </header>

  <ProjectBatchReclassificationEditor
    {rows}
    {projects}
    {readOnly}
    {onRefresh}
    {onComplete}
    {onOpenRules}
    onCandidateCount={(count) => (suggestionCount = count)}
  />
</section>

<style>
  .workspace {
    display: flex;
    height: 100%;
    min-height: 0;
    flex-direction: column;
  }

  .workspace-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
    padding: 13px 14px;
    border-bottom: 1px solid var(--border-muted);
  }

  .workspace-title {
    min-width: 0;
    flex: 1;
  }

  .title-line,
  .project-facts {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
  }

  .title-line { gap: 8px; }
  .project-facts {
    gap: var(--space-6);
    margin-top: 7px;
    color: var(--text-muted);
    font-size: 10px;
  }

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: 14px;
  }
</style>
