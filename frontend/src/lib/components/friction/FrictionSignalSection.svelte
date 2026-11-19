<script lang="ts">
  import type { FrictionDiagnosticGroup, FrictionIssueItem } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import FrictionFinding from "./FrictionFinding.svelte";
  import FrictionSessionRef from "./FrictionSessionRef.svelte";
  import {
    interruptionRows,
    isDiagnosticSubject,
    signalDims,
    type FrictionKind,
    type FrictionSessions,
    type SignalSection,
  } from "./frictionView.js";

  interface Props {
    section: SignalSection;
    sessions: FrictionSessions | null | undefined;
    // Kata issues by pattern fingerprint, shown beside each finding.
    issues?: Record<string, FrictionIssueItem>;
    // Diagnostic rows, grouped by the server as in the Markdown.
    diagnostics?: FrictionDiagnosticGroup[];
  }

  let { section, sessions, issues = {}, diagnostics = [] }: Props = $props();

  const TITLES: Record<FrictionKind, () => string> = {
    correction: m.friction_section_corrections,
    error: m.friction_section_errors,
    workaround: m.friction_section_workarounds,
    deferral: m.friction_section_deferrals,
    pattern: m.friction_section_patterns,
    frustration: m.friction_section_frustration,
    interruption: m.friction_section_interruptions,
    diagnostic: m.friction_section_diagnostics,
  };

  // Interruptions render one row per session with a count, as in the
  // Markdown.
  const rows = $derived.by(() => {
    if (section.kind === "interruption") return interruptionRows(section.signals);
    return section.signals.map((first) => ({ first, count: 1 }));
  });
</script>

<section
  class="friction-section"
  id={`friction-section-${section.kind}`}
  aria-labelledby={`friction-section-${section.kind}-title`}
>
  <h2 id={`friction-section-${section.kind}-title`}>
    {TITLES[section.kind]()}
    <span class="count">{section.signals.length}</span>
  </h2>
  <ul class="signal-list">
    {#if section.kind === "diagnostic"}
      <!-- Diagnostics have no session to open and are never filed. -->
      {#each diagnostics as group}
        <li class="signal-row" data-kind="diagnostic">
          <FrictionFinding
            finding={{ kind: "diagnostic", label: group.source, text: group.message, tool_name: "", evidence: "" }}
            count={group.count}
            showCount={diagnostics.length > 1}
          />
        </li>
      {/each}
    {:else}
      {#each rows as { first: signal, count }}
        <li class="signal-row" data-kind={signal.kind}>
          <FrictionFinding finding={signal} subjectId={signal.subject_id} {count} />
          <span class="row-context">
            <FrictionSessionRef
              subjectId={signal.subject_id}
              {sessions}
              diagnostic={isDiagnosticSubject(signal)}
              ordinal={signal.message_ordinal}
              dims={signalDims(signal)}
            />
            {#if issues[signal.fingerprint]}
              {@const issue = issues[signal.fingerprint]!}
              {#if issue.web_url}
                <a class="row-issue" href={issue.web_url} target="_blank" rel="noopener noreferrer">
                  {issue.qualified_id}
                </a>
              {:else}
                <span class="row-issue">{issue.qualified_id}</span>
              {/if}
            {/if}
          </span>
        </li>
      {/each}
    {/if}
  </ul>
</section>

<style>
  .friction-section {
    display: flex;
    flex-direction: column;
    gap: 6px;
    scroll-margin-top: 12px;
  }
  .friction-section h2 {
    margin: 0;
    font-size: 14px;
    display: flex;
    align-items: baseline;
    gap: 8px;
  }
  .count {
    color: var(--text-muted);
    font-weight: 500;
  }
  .signal-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
  }
  .signal-row {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: 8px 4px;
    border-bottom: 1px solid var(--border-muted);
    font-size: 13px;
    line-height: 1.45;
  }
  .signal-row:last-child {
    border-bottom: none;
  }
  .row-context {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    column-gap: var(--space-4);
  }
  .row-issue {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--accent-blue);
  }
</style>
