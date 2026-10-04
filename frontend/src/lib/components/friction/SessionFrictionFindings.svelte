<script module lang="ts">
  export const SESSION_FINDINGS_LIMIT = 500;
</script>

<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { FrictionService, type FrictionFindingItem } from "../../api/generated/index.js";
  import { isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { findingSummary, type FrictionKind } from "./frictionView.js";

  interface Props {
    sessionId: string;
  }

  const KIND_LABELS: Record<FrictionKind, () => string> = {
    correction: m.friction_kind_correction,
    error: m.friction_kind_error,
    workaround: m.friction_kind_workaround,
    deferral: m.friction_kind_deferral,
    pattern: m.friction_kind_pattern,
    frustration: m.friction_kind_frustration,
    interruption: m.friction_kind_interruption,
  };

  function kindLabel(kind: string): string {
    const label = KIND_LABELS[kind as FrictionKind];
    return label ? label() : kind;
  }

  let { sessionId }: Props = $props();
  let findings = $state<FrictionFindingItem[]>([]);
  let loading = $state(true);
  let failed = $state(false);
  const read = new LatestRead();

  async function load(id: string) {
    const signal = read.begin();
    loading = true;
    failed = false;
    try {
      const res = await FrictionService.getApiV1FrictionFindings(
        { session_id: id, limit: SESSION_FINDINGS_LIMIT },
        { signal },
      );
      if (!read.isCurrent(signal)) return;
      findings = res.findings ?? [];
    } catch (error) {
      if (isAbortError(error) || !read.isCurrent(signal)) return;
      findings = [];
      failed = true;
    } finally {
      if (read.finish(signal)) loading = false;
    }
  }

  $effect(() => {
    const id = sessionId;
    void load(id);
    return () => read.cancel();
  });
</script>

<section class="friction-findings" aria-labelledby="session-friction-title">
  <header class="findings-header">
    <span id="session-friction-title">{m.friction_session_title()}</span>
    {#if !loading && !failed}
      <span class="findings-count">{findings.length}</span>
    {/if}
  </header>

  {#if loading}
    <p class="findings-state">{m.friction_session_loading()}</p>
  {:else if failed}
    <p class="findings-state">{m.friction_session_error()}</p>
  {:else if findings.length === 0}
    <p class="findings-state">{m.friction_session_empty()}</p>
  {:else}
    <ul class="findings-list">
      {#each findings as finding}
        <li class="finding-row" data-kind={finding.kind}>
          <span class="kind">{kindLabel(finding.kind)}</span>
          <code class="detector">{finding.detector}</code>
          {#if findingSummary(finding)}
            <span class="finding-text">{findingSummary(finding)}</span>
          {/if}
          {#if finding.message_ordinal != null}
            <Button
              size="sm"
              tone="neutral"
              surface="outline"
              label={m.friction_session_jump({ ordinal: finding.message_ordinal })}
              title={m.friction_session_jump_title({ ordinal: finding.message_ordinal })}
              onclick={() => ui.scrollToOrdinal(finding.message_ordinal!, sessionId)}
            />
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .friction-findings {
    margin-top: 6px;
    padding-top: 6px;
    border-top: 1px solid var(--border-muted);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .findings-header {
    display: flex;
    gap: 6px;
    color: var(--text-secondary);
    font-weight: 600;
  }
  .findings-count {
    color: var(--text-muted);
    font-weight: 500;
  }
  .findings-state {
    margin: 0;
    color: var(--text-muted);
    font-style: italic;
  }
  .findings-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .finding-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }
  .kind {
    font-weight: 600;
    color: var(--text-secondary);
  }
  .detector {
    color: var(--accent-amber);
    font-size: 11px;
  }
  .finding-text {
    color: var(--text-secondary);
    word-break: break-word;
    flex: 1;
    min-width: 12ch;
  }
</style>
