<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import type { DbSignalSessionExample as SignalSessionExample } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { agentLabel } from "../../utils/agents.js";
  import { errorTelemetry } from "../../utils/error-telemetry.js";
  import { formatCount } from "../../utils/format.js";
  import type { SignalEvidence } from "./signalEvidence.svelte.js";

  // The sessions behind the quality signal someone picked. The page shows
  // this only while a signal is selected.
  interface Props {
    evidence: SignalEvidence;
    label: string;
  }

  let { evidence, label }: Props = $props();

  function openEvidenceSession(
    example: SignalSessionExample,
    event: MouseEvent,
  ) {
    event.preventDefault();
    const params = evidenceSessionParams(example);
    // Route-first: App's deep-link effect owns selection and
    // hydration once the URL commits (#1190).
    router.navigateToSession(example.session_id, params);
    if (example.message_ordinal != null) {
      ui.scrollToOrdinal(example.message_ordinal, example.session_id);
    }
  }

  function openEvidenceListSession(event: MouseEvent) {
    if (
      !(event.target instanceof Element) ||
      !(event.currentTarget instanceof HTMLElement)
    ) return;
    const link = event.target.closest<HTMLAnchorElement>("a.evidence-row");
    if (!link) return;
    const links = Array.from(
      event.currentTarget.querySelectorAll<HTMLAnchorElement>(
        "a.evidence-row",
      ),
    );
    const example = evidence.examples[links.indexOf(link)];
    if (!example) return;
    openEvidenceSession(example, event);
  }

  function delegateEvidenceClicks(node: HTMLElement) {
    const handleClick = (event: MouseEvent) => {
      openEvidenceListSession(event);
    };
    node.addEventListener("click", handleClick);
    return {
      destroy() {
        node.removeEventListener("click", handleClick);
      },
    };
  }

  function evidenceSessionParams(
    example: SignalSessionExample,
  ): Record<string, string> {
    return example.message_ordinal == null
      ? {}
      : { msg: String(example.message_ordinal) };
  }

  function qualityBadge(example: SignalSessionExample): string {
    if (example.health_grade) return m.insights_page_grade_badge({ grade: example.health_grade });
    if (example.health_score != null) return String(example.health_score);
    return m.insights_page_unscored();
  }
</script>

<Card level="default" padding="none" class="evidence-panel">
  <section class="evidence-panel-live" aria-live="polite">
    <div class="evidence-head">
    <div>
      <span class="examples-label">{m.insights_page_session_evidence()}</span>
      <h3>{label}</h3>
    </div>
    <button
      class="text-btn"
      type="button"
      onclick={() => evidence.close()}
    >
      {m.insights_page_close()}
    </button>
    </div>
    {#if evidence.loading}
      <p class="evidence-state">{m.insights_page_loading_examples()}</p>
    {:else if evidence.error}
      <p class="evidence-state error" {@attach errorTelemetry("analytics")}>{evidence.error}</p>
    {:else if evidence.examples.length === 0}
      <p class="evidence-state">
        {m.insights_page_no_triggering_sessions()}
      </p>
    {:else}
      <div class="evidence-list" use:delegateEvidenceClicks>
        {#each evidence.examples as example}
          <Card
            level="inset"
            padding="none"
            class="evidence-row"
            href={router.buildSessionHref(
            example.session_id,
            evidenceSessionParams(example),
          )}
          >
            <span class="evidence-main">
              <strong>{example.project || m.insights_page_unassigned_project()}</strong>
              <em>{example.excerpt || m.insights_page_no_excerpt()}</em>
            </span>
            <span class="evidence-meta">
              <span>{agentLabel(example.agent)}</span>
              <span>{example.outcome || m.insights_page_unknown()}</span>
              <span>{qualityBadge(example)}</span>
              {#if example.failure_signals != null}<span>{m.insights_page_failures({ count: formatCount(example.failure_signals) })}</span>{/if}
            </span>
          </Card>
        {/each}
      </div>
    {/if}
  </section>
</Card>

<style>
  .examples-label {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .evidence-panel-live {
    display: grid;
    gap: var(--space-5);
    min-width: 0;
  }

  .evidence-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .evidence-head h3 {
    margin-top: 2px;
    color: var(--text-primary);
    font-size: 14px;
    font-weight: var(--font-weight-semibold, 600);
  }

  .evidence-state {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .evidence-state.error {
    color: var(--accent-red);
  }

  .evidence-list {
    display: grid;
    gap: 6px;
  }

  .evidence-list :global(.evidence-row) {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 12px;
    align-items: center;
    padding: 9px 10px;
    text-decoration: none;
  }

  .evidence-list :global(.evidence-row > .kit-card__body) {
    display: contents;
  }

  .evidence-list :global(.evidence-row:hover) {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .evidence-main {
    display: grid;
    gap: 2px;
    min-width: 0;
  }

  .evidence-main strong {
    color: var(--text-primary);
    font-size: 12px;
  }

  .evidence-main em {
    color: var(--text-muted);
    font-size: 12px;
    font-style: normal;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .evidence-meta {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 6px;
    color: var(--text-muted);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
  }

  .text-btn {
    color: var(--text-muted);
    font-size: 12px;
  }

  .text-btn:hover {
    color: var(--text-primary);
  }


  @media (max-width: 900px) {
    .evidence-list :global(.evidence-row) {
      grid-template-columns: 1fr;
    }

    .evidence-meta {
      justify-content: flex-start;
    }
  }
</style>
