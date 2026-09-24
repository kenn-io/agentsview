<script lang="ts">
  import type { FrictionSignal } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import {
    findingSummary,
    interruptionRows,
    isDiagnosticSubject,
    sessionLinkParams,
    signalDims,
    type FrictionKind,
    type SignalSection,
  } from "./frictionView.js";

  interface Props {
    section: SignalSection;
  }

  let { section }: Props = $props();

  const TITLES: Record<FrictionKind, () => string> = {
    correction: m.friction_section_corrections,
    error: m.friction_section_errors,
    workaround: m.friction_section_workarounds,
    deferral: m.friction_section_deferrals,
    pattern: m.friction_section_patterns,
    frustration: m.friction_section_frustration,
    interruption: m.friction_section_interruptions,
  };
  const EMPTY: Record<FrictionKind, () => string> = {
    correction: m.friction_none_corrections,
    error: m.friction_none_errors,
    workaround: m.friction_none_workarounds,
    deferral: m.friction_none_deferrals,
    pattern: m.friction_none_patterns,
    frustration: m.friction_none_frustration,
    interruption: m.friction_none_interruptions,
  };

  // Spec §9.1: interruptions render one row per session with a count.
  const interruptions = $derived(
    section.kind === "interruption" ? interruptionRows(section.signals) : [],
  );

  function linkTitle(signal: FrictionSignal): string {
    return signal.message_ordinal == null
      ? m.friction_open_session_start({ session: signal.subject_id })
      : m.friction_open_session({ session: signal.subject_id, ordinal: signal.message_ordinal });
  }

  function openSignal(signal: FrictionSignal, event: MouseEvent) {
    if (
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    // Route-first, as QualityPage evidence links do: App's deep-link effect
    // owns selection and hydration once the URL commits.
    router.navigateToSession(signal.subject_id, sessionLinkParams(signal));
    if (signal.message_ordinal != null) {
      ui.scrollToOrdinal(signal.message_ordinal, signal.subject_id);
    }
  }
</script>

<section class="friction-section" aria-labelledby={`friction-section-${section.kind}`}>
  <h2 id={`friction-section-${section.kind}`}>
    {TITLES[section.kind]()}
    <span class="count">{section.signals.length}</span>
  </h2>
  {#snippet subject(signal: FrictionSignal)}
    {#if isDiagnosticSubject(signal)}
      <code class="subject">{signal.subject_id}</code>
    {:else}
      <a
        class="subject-link"
        href={router.buildSessionHref(signal.subject_id, sessionLinkParams(signal))}
        title={linkTitle(signal)}
        onclick={(event) => openSignal(signal, event)}
      >
        <code>{signal.subject_id}</code>
      </a>
    {/if}
  {/snippet}

  {#if section.signals.length === 0}
    <p class="placeholder">{EMPTY[section.kind]()}</p>
  {:else if section.kind === "interruption"}
    <ul class="signal-list">
      {#each interruptions as row (row.subjectId)}
        <li class="signal-row" data-kind="interruption">
          {#each signalDims(row.first) as dim}
            <span class="dim">{dim}</span>
          {/each}
          {@render subject(row.first)}
          <span class="detail">{m.friction_interruption_count({ count: row.count })}</span>
        </li>
      {/each}
    </ul>
  {:else}
    <ul class="signal-list">
      {#each section.signals as signal}
        <li class="signal-row" data-kind={signal.kind}>
          {#each signalDims(signal) as dim}
            <span class="dim">{dim}</span>
          {/each}
          {@render subject(signal)}
          <span class="detail">{findingSummary(signal)}</span>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .friction-section {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .friction-section h2 {
    margin: 0;
    font-size: 14px;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .count {
    color: var(--text-muted);
    font-weight: 500;
  }
  .placeholder {
    margin: 0;
    color: var(--text-muted);
    font-style: italic;
  }
  .signal-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .signal-row {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 6px;
    padding: 6px 8px;
    border-bottom: 1px solid var(--border-muted);
    font-size: 12px;
  }
  .dim {
    padding: 1px 6px;
    border-radius: 3px;
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    color: var(--accent-blue);
    font-size: 11px;
  }
  .subject-link {
    color: var(--accent-blue);
  }
  .detail {
    color: var(--text-secondary);
    white-space: pre-wrap;
    word-break: break-word;
    display: -webkit-box;
    -webkit-line-clamp: 4;
    line-clamp: 4;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
</style>
