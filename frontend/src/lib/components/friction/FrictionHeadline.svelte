<script lang="ts">
  import { Card, Chip } from "@kenn-io/kit-ui";
  import type {
    FrictionP0Alert,
    FrictionPatternItem,
    FrictionSignal,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import FrictionFinding from "./FrictionFinding.svelte";
  import FrictionIssueControl from "./FrictionIssueControl.svelte";
  import FrictionSessionRef from "./FrictionSessionRef.svelte";
  import {
    headlinePatterns,
    isDiagnosticSubject,
    sortedP0,
    type FrictionKind,
    type FrictionSessions,
  } from "./frictionView.js";

  interface Props {
    date: string;
    p0Alerts: FrictionP0Alert[] | null | undefined;
    signals: FrictionSignal[];
    patterns: FrictionPatternItem[];
    sessions: FrictionSessions | null | undefined;
    // This page may file to Kata: writes are allowed and Kata is ready.
    canFile?: boolean;
  }

  let { date, p0Alerts, signals, patterns, sessions, canFile = false }: Props = $props();

  const KIND_LABELS: Record<FrictionKind, () => string> = {
    correction: m.friction_kind_correction,
    error: m.friction_kind_error,
    workaround: m.friction_kind_workaround,
    deferral: m.friction_kind_deferral,
    pattern: m.friction_kind_pattern,
    frustration: m.friction_kind_frustration,
    interruption: m.friction_kind_interruption,
  };

  const p0 = $derived(sortedP0(p0Alerts));
  const ranked = $derived(headlinePatterns(date, signals, patterns));
  const groups = $derived(
    [
      { id: "new", title: m.friction_patterns_new(), rows: ranked.fresh },
      { id: "recurring", title: m.friction_patterns_recurring(), rows: ranked.recurring },
    ].filter((group) => group.rows.length > 0),
  );
</script>

{#if p0.length > 0 || groups.length > 0}
  <section class="friction-headline" aria-labelledby="friction-headline-title">
    <h2 id="friction-headline-title">{m.friction_headline_title()}</h2>

    {#if p0.length > 0}
      <Card level="default" padding="none" class="headline-card">
        <div class="headline-block">
          <h3>{m.friction_p0_title()}</h3>
          <ul class="headline-list">
            {#each p0 as alert (alert.tool)}
              <li class="headline-row">
                <span class="row-main">
                  <Chip size="xs" tone="danger">{alert.tool}</Chip>
                  {m.friction_p0_failed({ count: alert.subject_ids.length })}
                </span>
                <ul class="alert-sessions">
                  {#each alert.subject_ids as id (id)}
                    <li><FrictionSessionRef subjectId={id} {sessions} /></li>
                  {/each}
                </ul>
              </li>
            {/each}
          </ul>
        </div>
      </Card>
    {/if}

    {#each groups as group (group.id)}
      <Card level="default" padding="none" class="headline-card">
        <div class="headline-block">
          <h3>{group.title}</h3>
          <ul class="headline-list">
            {#each group.rows as p (p.fingerprint)}
              <li class="headline-row">
                <span class="row-main">
                  <Chip size="xs" tone={group.id === "new" ? "info" : "warning"}>
                    {KIND_LABELS[p.kind as FrictionKind]?.() ?? p.kind}
                  </Chip>
                  {#if p.anchor}
                    <FrictionFinding finding={p.anchor} subjectId={p.anchor.subject_id} />
                  {:else}
                    <span>{p.title}</span>
                  {/if}
                </span>
                <span class="row-meta">
                  <span>
                    {m.friction_pattern_occurrences({ count: p.occurrence_count })}
                    · {m.friction_pattern_sessions({ count: p.session_count })}
                    · {m.friction_pattern_first_seen({ date: p.first_seen_date })}
                  </span>
                  {#if p.anchor}
                    <FrictionSessionRef
                      subjectId={p.anchor.subject_id}
                      {sessions}
                      diagnostic={isDiagnosticSubject(p.anchor)}
                      ordinal={p.anchor.message_ordinal}
                    />
                  {/if}
                </span>
                {#if p.issue || p.filing || canFile}
                  <FrictionIssueControl
                    fingerprint={p.fingerprint}
                    kind={p.kind}
                    issue={p.issue}
                    filing={p.filing}
                    {canFile}
                  />
                {/if}
              </li>
            {/each}
          </ul>
        </div>
      </Card>
    {/each}
  </section>
{/if}

<style>
  .friction-headline {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .friction-headline h2 {
    margin: 0;
    font-size: 15px;
  }
  .headline-block {
    padding: 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .headline-block h3 {
    margin: 0;
    font-size: 13px;
  }
  .headline-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }
  .headline-row {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: 13px;
    line-height: 1.45;
  }
  .row-main {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 6px;
  }
  .row-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    column-gap: var(--space-5);
    font-size: 12px;
    color: var(--text-muted);
  }
  .alert-sessions {
    list-style: none;
    margin: 0;
    padding: 0 0 0 4px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
</style>
