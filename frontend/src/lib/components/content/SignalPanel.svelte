<script lang="ts">
  import { m } from "../../i18n/index.js";
  import type { Session } from "../../api/types/core.js";
  import SessionFrictionFindings from "../friction/SessionFrictionFindings.svelte";
  import {
    getGradeStyle,
    getGradeLabel,
    getOutcomeColor,
    getOutcomeLabel,
    getPenaltyLabel,
    getBasisLabel,
  } from "../../utils/grade.js";
  import {
    CheckIcon,
    CircleQuestionMarkIcon,
    TriangleAlertIcon,
    XIcon,
  } from "../../icons.js";

  interface Props {
    session: Session;
  }

  let { session }: Props = $props();

  const gradeStyle = $derived(
    getGradeStyle(session.health_grade),
  );
  const basis = $derived(session.health_score_basis ?? []);
  const penalties = $derived(session.health_penalties);
  const hasPenalties = $derived(
    penalties != null && Object.keys(penalties).length > 0,
  );
  // JSON key order is not stable across loads, so list the largest
  // deduction first and break ties by key.
  const sortedPenalties = $derived(
    Object.entries(penalties ?? {}).sort(
      ([aKey, aValue], [bKey, bValue]) =>
        bValue - aValue || (aKey < bKey ? -1 : aKey > bKey ? 1 : 0),
    ),
  );
  const compactions = $derived(session.compaction_count ?? 0);
  const midTaskCompactions = $derived(
    session.mid_task_compaction_count ?? 0,
  );
  const showCompactionChip = $derived(compactions > 0);

  const outcome = $derived(session.outcome ?? "unknown");
  const outcomeLabel = $derived(getOutcomeLabel(outcome));
  const confidence = $derived(session.outcome_confidence ?? "");

  // The panel is opt-in (toggled from the breadcrumb), so when
  // the session has no useful signal data we still render
  // something — just a brief "no data" line instead of empty
  // chrome that looks like a debug dump.
  const hasUsefulData = $derived(
    session.health_score != null ||
      outcome !== "unknown" ||
      hasPenalties ||
      compactions > 0 ||
      basis.length > 1,
  );
</script>

<div class="signal-panel">
  {#if !hasUsefulData}
    <div class="no-signal-note">
      {m.signal_panel_not_enough_activity()}
    </div>
  {:else}
    <div class="signal-row">
      <span
        class="grade-large"
        style:color={gradeStyle.text}
        title={m.signal_panel_health_grade({ grade: getGradeLabel(session.health_grade) })}
      >
        {getGradeLabel(session.health_grade)}
      </span>
      <span class="score-text">
        {session.health_score ?? "--"} / 100
      </span>

      <span
        class="outcome"
        style:color={getOutcomeColor(outcome)}
        title={confidence
          ? m.signal_panel_outcome_confidence({ outcome: outcomeLabel, confidence })
          : outcomeLabel}
        aria-label={outcomeLabel}
      >
        <span class="outcome-icon" aria-hidden="true">
          {#if outcome === "completed"}
            <CheckIcon size="14" strokeWidth="2.4" />
          {:else if outcome === "abandoned"}
            <TriangleAlertIcon size="14" strokeWidth="2" />
          {:else if outcome === "errored"}
            <XIcon size="14" strokeWidth="2.4" />
          {:else}
            <CircleQuestionMarkIcon size="14" strokeWidth="2" />
          {/if}
        </span>
      </span>

      {#if basis.length > 0}
        <span class="basis-tags">
          <span class="basis-heading">{m.signal_panel_based_on()}</span>
          {#each basis as b}
            <span
              class="basis-tag"
              title={m.signal_panel_basis_title({ factor: getBasisLabel(b) })}
            >
              {getBasisLabel(b)}
            </span>
          {/each}
        </span>
      {/if}

      {#if showCompactionChip}
        <span
          class="compaction-chip"
          class:mid-task={midTaskCompactions > 0}
          title={midTaskCompactions > 0
            ? m.signal_panel_compaction_interrupted({ mid: midTaskCompactions, total: compactions })
            : m.signal_panel_compaction_title()}
        >
          {m.signal_panel_compaction_count({ count: compactions })}
          {#if midTaskCompactions > 0}
            &middot; {m.signal_panel_mid_task({ count: midTaskCompactions })}
          {/if}
        </span>
      {/if}

      {#if !hasPenalties}
        <span class="no-penalties">{m.signal_panel_no_penalties()}</span>
      {/if}
    </div>

    {#if hasPenalties}
      <div class="penalties-row">
        <span class="penalties-heading">{m.signal_panel_penalties()}</span>
        {#each sortedPenalties as [key, value] (key)}
          <div
            class="penalty"
            title={m.signal_panel_penalty_title({ label: getPenaltyLabel(key), value })}
          >
            <span class="penalty-value">-{value}</span>
            <span class="penalty-label">
              {getPenaltyLabel(key)}
            </span>
          </div>
        {/each}
      </div>
    {/if}
  {/if}
  <SessionFrictionFindings sessionId={session.id} />
</div>

<style>
  /* The panel sits on the transcript's shared reading column (see
     MessageList .virtual-row): its fill and bottom rule span the column,
     not the pane. ToolSequencesPanel continues the same block below. */
  .signal-panel {
    margin-inline: max(12px, calc((100% - var(--reading-column)) / 2));
    padding: 10px 12px;
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-muted);
    font-size: 12px;
  }

  /* Dark inset sits within a step of the page, so the panel would melt into
     the transcript; one step above the card surface keeps it a distinct
     band, as the inset fill does in light. */
  :global(:root.dark) .signal-panel {
    background: color-mix(in srgb, var(--text-primary) 4%, var(--bg-surface));
  }
  .signal-row {
    display: flex;
    gap: 12px;
    align-items: center;
    flex-wrap: wrap;
  }
  .no-signal-note {
    color: var(--text-muted);
  }
  .grade-large {
    font-size: 20px;
    font-weight: 800;
  }
  /* The numeric score is the grade's partner; the basis chips and
     penalties stay subordinate. */
  .score-text {
    color: var(--text-primary);
    font-size: 14px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  .outcome {
    display: inline-flex;
    align-items: center;
    cursor: help;
  }
  .outcome-icon {
    display: inline-flex;
    align-items: center;
  }
  .basis-tags {
    display: flex;
    gap: 4px;
    align-items: center;
  }
  .basis-heading {
    color: var(--text-secondary);
    font-weight: 600;
    margin-right: 2px;
  }
  /* Neutral, so the factors read as labels rather than links. */
  .basis-tag {
    padding: 0 6px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    color: var(--text-secondary);
    border-radius: 3px;
    font-size: 11px;
    cursor: help;
  }
  .compaction-chip {
    padding: 1px 6px;
    background: color-mix(
      in srgb, var(--accent-amber) 18%, transparent
    );
    color: var(--accent-amber);
    border-radius: 3px;
    font-size: 11px;
  }
  .compaction-chip.mid-task {
    background: color-mix(
      in srgb, var(--accent-red) 18%, transparent
    );
    color: var(--accent-red);
  }
  .no-penalties {
    color: var(--text-muted);
  }
  .penalties-row {
    display: flex;
    gap: 12px;
    flex-wrap: wrap;
    align-items: center;
    padding-top: 6px;
    margin-top: 6px;
    border-top: 1px solid var(--border-muted);
  }
  .penalties-heading {
    color: var(--text-secondary);
    font-weight: 600;
  }
  .penalty {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .penalty-value {
    color: var(--accent-red);
    font-weight: 600;
  }
  .penalty-label {
    color: var(--text-secondary);
  }
</style>
