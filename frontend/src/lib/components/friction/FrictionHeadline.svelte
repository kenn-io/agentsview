<script lang="ts">
  import { Card, Chip } from "@kenn-io/kit-ui";
  import type {
    FrictionP0Alert,
    FrictionPatternItem,
    FrictionSignal,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import {
    headlinePatterns,
    isDiagnosticSubject,
    sessionLinkParams,
    sortedP0,
    type HeadlinePattern,
  } from "./frictionView.js";

  interface Props {
    date: string;
    p0Alerts: FrictionP0Alert[] | null | undefined;
    signals: FrictionSignal[];
    patterns: FrictionPatternItem[];
  }

  let { date, p0Alerts, signals, patterns }: Props = $props();

  const p0 = $derived(sortedP0(p0Alerts));
  const ranked = $derived(headlinePatterns(date, signals, patterns));
  const groups = $derived([
    { id: "new", title: m.friction_patterns_new(), rows: ranked.fresh },
    { id: "recurring", title: m.friction_patterns_recurring(), rows: ranked.recurring },
  ]);

  // A row links to the pattern's first occurrence in this digest (its
  // anchor), so the link always matches the digest being read. Diagnostic
  // anchors are identities, not sessions, and stay unlinked.
  function linkable(p: HeadlinePattern): boolean {
    return p.anchor !== null && !isDiagnosticSubject(p.anchor);
  }

  function openPattern(p: HeadlinePattern, event: MouseEvent) {
    if (
      !p.anchor ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    router.navigateToSession(p.anchor.subject_id, sessionLinkParams(p.anchor));
    if (p.anchor.message_ordinal != null) {
      ui.scrollToOrdinal(p.anchor.message_ordinal, p.anchor.subject_id);
    }
  }
</script>

<section class="friction-headline" aria-labelledby="friction-headline-title">
  <h2 id="friction-headline-title">{m.friction_headline_title()}</h2>

  <Card level="default" padding="none" class="headline-card">
    <div class="headline-block">
      <h3>{m.friction_p0_title()}</h3>
      {#if p0.length === 0}
        <p class="placeholder">{m.friction_p0_none()}</p>
      {:else}
        <ul class="p0-list">
          {#each p0 as alert (alert.tool)}
            <li>
              <Chip size="xs" tone="danger">P0</Chip>
              <span>{m.friction_p0_item({ tool: alert.tool, count: alert.subject_ids.length })}</span>
              <span class="subjects">{alert.subject_ids.join(", ")}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </Card>

  {#if ranked.fresh.length === 0 && ranked.recurring.length === 0}
    <p class="placeholder">{m.friction_patterns_none()}</p>
  {:else}
    {#each groups as group (group.id)}
      {#if group.rows.length > 0}
        <Card level="default" padding="none" class="headline-card">
          <div class="headline-block">
            <h3>{group.title}</h3>
            <ul class="pattern-list">
              {#each group.rows as p (p.fingerprint)}
                <li class="pattern-row">
                  <Chip size="xs" tone={p.isNew ? "info" : "warning"}>{p.kind}</Chip>
                  {#if linkable(p) && p.anchor}
                    <a
                      class="pattern-title"
                      href={router.buildSessionHref(p.anchor.subject_id, sessionLinkParams(p.anchor))}
                      onclick={(event) => openPattern(p, event)}
                    >
                      {p.title}
                    </a>
                  {:else}
                    <span class="pattern-title">{p.title}</span>
                  {/if}
                  <span class="pattern-meta">
                    {m.friction_pattern_occurrences({ count: p.occurrence_count })}
                    · {m.friction_pattern_sessions({ count: p.session_count })}
                    · {m.friction_pattern_first_seen({ date: p.first_seen_date })}
                  </span>
                </li>
              {/each}
            </ul>
          </div>
        </Card>
      {/if}
    {/each}
  {/if}
</section>

<style>
  .friction-headline {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }
  .friction-headline h2 {
    margin: 0;
    font-size: 15px;
  }
  .headline-block {
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .headline-block h3 {
    margin: 0;
    font-size: 13px;
  }
  .p0-list,
  .pattern-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 12px;
  }
  .p0-list li,
  .pattern-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .subjects,
  .pattern-meta {
    color: var(--text-muted);
  }
  .pattern-title {
    color: var(--accent-blue);
    word-break: break-word;
  }
  span.pattern-title {
    color: var(--text-primary);
  }
  .placeholder {
    margin: 0;
    color: var(--text-muted);
    font-style: italic;
  }
</style>
