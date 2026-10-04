<script lang="ts">
  import { Button, Card, Chip, Modal, TextInput } from "@kenn-io/kit-ui";
  import type {
    FrictionP0Alert,
    FrictionPatternItem,
    FrictionSignal,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { friction } from "../../stores/friction.svelte.js";
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
    kataAvailable: boolean;
  }

  let { date, p0Alerts, signals, patterns, kataAvailable }: Props = $props();
  let linkingFingerprint = $state<string | null>(null);
  let issueRef = $state("");
  let issueRequired = $state(false);

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

  function stateLabel(state: string): string {
    switch (state) {
      case "linked": return m.friction_kata_state_linked();
      case "pending": return m.friction_kata_state_pending();
      case "failed": return m.friction_kata_state_failed();
      case "needs_human": return m.friction_kata_state_needs_human();
      case "abandoned": return m.friction_kata_state_abandoned();
      default: return m.friction_kata_state_unknown();
    }
  }

  function stateTone(state: string): "success" | "info" | "warning" | "danger" | "muted" {
    switch (state) {
      case "linked": return "success";
      case "pending": return "info";
      case "needs_human": return "warning";
      case "failed": case "abandoned": return "danger";
      default: return "muted";
    }
  }

  function issueURL(raw: string): string | null {
    try {
      const url = new URL(raw);
      return url.protocol === "https:" || url.protocol === "http:" ? url.toString() : null;
    } catch {
      return null;
    }
  }

  function openLink(fingerprint: string): void {
    linkingFingerprint = fingerprint;
    issueRef = "";
    issueRequired = false;
  }

  async function submitLink(): Promise<void> {
    const fingerprint = linkingFingerprint;
    if (!fingerprint || friction.mutationFingerprint !== null) return;
    const ref = issueRef.trim();
    if (!ref) {
      issueRequired = true;
      return;
    }
    issueRequired = false;
    await friction.linkPattern(fingerprint, ref);
    if (friction.mutationError === null) linkingFingerprint = null;
  }
</script>

{#snippet linkActions()}
  <Button label={m.friction_kata_cancel()} size="sm" onclick={() => (linkingFingerprint = null)} />
  <Button
    label={m.friction_kata_link()}
    size="sm"
    tone="info"
    surface="solid"
    disabled={friction.mutationFingerprint !== null}
    onclick={() => void submitLink()}
  />
{/snippet}

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
                  {#if p.link}
                    <Chip size="xs" tone={stateTone(p.link.state)} uppercase={false}>
                      {stateLabel(p.link.state)}
                    </Chip>
                    {#if p.link.qualified_id}
                      {@const url = issueURL(p.link.web_url)}
                      {#if url}
                        <a class="kata-issue" href={url} target="_blank" rel="noopener noreferrer">
                          {p.link.qualified_id}
                        </a>
                      {:else}
                        <span class="kata-issue">{p.link.qualified_id}</span>
                      {/if}
                    {/if}
                  {/if}
                  {#if kataAvailable}
                    {#if p.link?.state === "linked"}
                      <Button
                        size="sm"
                        label={m.friction_kata_unlink()}
                        title={m.friction_kata_unlink_title()}
                        disabled={friction.mutationFingerprint !== null}
                        onclick={() => void friction.unlinkPattern(p.fingerprint)}
                      />
                    {:else}
                      {#if !p.link || p.link.state === "failed" || p.link.state === "abandoned"}
                        <Button
                          size="sm"
                          label={m.friction_kata_file()}
                          disabled={friction.mutationFingerprint !== null}
                          onclick={() => void friction.filePattern(p.fingerprint)}
                        />
                      {/if}
                      <Button
                        size="sm"
                        label={m.friction_kata_link()}
                        disabled={friction.mutationFingerprint !== null}
                        onclick={() => openLink(p.fingerprint)}
                      />
                    {/if}
                  {/if}
                  {#if friction.mutationError?.fingerprint === p.fingerprint}
                    <span class="kata-error" role="alert">{friction.mutationError.message}</span>
                  {/if}
                </li>
              {/each}
            </ul>
          </div>
        </Card>
      {/if}
    {/each}
  {/if}
</section>

{#if linkingFingerprint !== null}
  <Modal
    title={m.friction_kata_link_title()}
    closeLabel={m.friction_kata_cancel()}
    width="440px"
    onclose={() => (linkingFingerprint = null)}
    footer={linkActions}
  >
    <p class="kata-link-hint">{m.friction_kata_link_hint()}</p>
    <TextInput
      id="friction-kata-issue"
      bind:value={issueRef}
      ariaLabel={m.friction_kata_issue_label()}
      placeholder={m.friction_kata_issue_placeholder()}
      invalid={issueRequired}
      block
      autofocus
      onkeydown={(event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          void submitLink();
        }
      }}
    />
    {#if issueRequired}
      <p class="kata-error" role="alert">{m.friction_kata_issue_required()}</p>
    {/if}
    {#if friction.mutationError?.fingerprint === linkingFingerprint}
      <p class="kata-error" role="alert">{friction.mutationError.message}</p>
    {/if}
  </Modal>
{/if}

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
  .kata-issue { color: var(--accent-blue); }
  .kata-error { color: var(--accent-red); }
  .kata-link-hint { margin: 0 0 12px; color: var(--text-secondary); font-size: 13px; }
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
