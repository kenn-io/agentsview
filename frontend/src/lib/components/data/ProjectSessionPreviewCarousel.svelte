<script lang="ts">
  import { Button, IconButton } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import {
    SessionsService,
    type DbWorktreeReclassificationSessionSample,
    type ServiceSessionDetail,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { ChevronDownIcon, ChevronLeftIcon, ChevronRightIcon } from "../../icons.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { displayProjectLabel } from "./project-label.js";

  interface Props {
    samples: DbWorktreeReclassificationSessionSample[];
  }

  let { samples }: Props = $props();

  let expanded = $state(false);
  let activeIndex = $state(0);
  let details = $state<Record<string, ServiceSessionDetail>>({});
  let loadingId = $state("");
  let loadError = $state("");
  const detailRead = new LatestRead();

  const activeSample = $derived(samples[activeIndex]);
  const activeDetail = $derived(activeSample ? details[activeSample.id] : undefined);
  const messagePreview = $derived(
    normalizeMessagePreview(activeDetail?.first_message) ||
      m.data_reclassify_session_preview_no_message(),
  );

  onDestroy(() => detailRead.cancel());

  function toggleExpanded() {
    expanded = !expanded;
    if (expanded && activeSample) void loadDetail(activeSample);
  }

  function move(offset: number) {
    const nextIndex = Math.max(0, Math.min(samples.length - 1, activeIndex + offset));
    if (nextIndex === activeIndex) return;
    activeIndex = nextIndex;
    const sample = samples[nextIndex];
    if (sample) void loadDetail(sample);
  }

  async function loadDetail(sample: DbWorktreeReclassificationSessionSample) {
    loadError = "";
    if (details[sample.id]) return;
    const signal = detailRead.begin();
    loadingId = sample.id;
    try {
      const detail = await callGenerated(
        () => SessionsService.getApiV1SessionsId({ id: sample.id }),
        signal,
      );
      if (!detailRead.isCurrent(signal)) return;
      details = { ...details, [sample.id]: detail };
    } catch (error) {
      if (isAbortError(error) || !detailRead.isCurrent(signal)) return;
      loadError = m.data_reclassify_session_preview_failed();
    } finally {
      if (detailRead.finish(signal)) loadingId = "";
    }
  }
</script>

<section class="session-previews">
  <Button
    size="sm"
    class="session-preview-toggle"
    surface="soft"
    label={m.data_reclassify_session_preview_count({ count: samples.length })}
    ariaExpanded={expanded}
    onclick={toggleExpanded}
  >
    {#snippet trailing()}
      {#if expanded}
        <ChevronDownIcon size="13" strokeWidth="2.2" aria-hidden="true" />
      {:else}
        <ChevronRightIcon size="13" strokeWidth="2.2" aria-hidden="true" />
      {/if}
    {/snippet}
  </Button>

  {#if expanded && activeSample}
    <div class="carousel" aria-live="polite">
      <div class="carousel-nav">
        <span>{m.data_reclassify_session_preview_position({ current: activeIndex + 1, count: samples.length })}</span>
        <div class="carousel-buttons">
          <IconButton
            size="sm"
            ariaLabel={m.data_reclassify_session_preview_previous()}
            disabled={activeIndex === 0}
            onclick={() => move(-1)}
          >
            <ChevronLeftIcon size="14" aria-hidden="true" />
          </IconButton>
          <IconButton
            size="sm"
            ariaLabel={m.data_reclassify_session_preview_next()}
            disabled={activeIndex === samples.length - 1}
            onclick={() => move(1)}
          >
            <ChevronRightIcon size="14" aria-hidden="true" />
          </IconButton>
        </div>
      </div>

      <div class="project-change">
        <div>
          <span>{m.data_reclassify_session_preview_current_project()}</span>
          <strong>{displayProjectLabel(activeSample.current_project)}</strong>
        </div>
        <ChevronRightIcon size="14" aria-hidden="true" />
        <div>
          <span>{m.data_reclassify_session_preview_next_project()}</span>
          <strong>{displayProjectLabel(activeSample.next_project)}</strong>
        </div>
      </div>

      {#if loadingId === activeSample.id}
        <p class="preview-status">{m.data_reclassify_session_preview_loading()}</p>
      {:else if loadError}
        <p class="preview-status error-text">{loadError}</p>
      {:else if activeDetail}
        <div class="session-copy">
          {#if activeDetail.display_name}
            <strong>{activeDetail.display_name}</strong>
          {/if}
          <p>{messagePreview}</p>
          <span>
            {activeDetail.agent_label || activeDetail.agent}
            {#if activeDetail.started_at}
              · {formatDateTime(activeDetail.started_at, { dateStyle: "medium", timeStyle: "short" })}
            {/if}
          </span>
        </div>
      {/if}

      <div class="session-folder">
        <span>{m.data_reclassify_session_preview_folder()}</span>
        <code>{activeSample.cwd}</code>
      </div>
    </div>
  {/if}
</section>

<style>
  .session-previews {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding-top: 10px;
    border-top: 1px solid var(--border-muted);
  }
  .session-previews :global(.session-preview-toggle) {
    width: 100%;
    justify-content: space-between;
  }
  .carousel {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }
  .carousel-nav,
  .carousel-buttons,
  .project-change {
    display: flex;
    align-items: center;
  }
  .carousel-nav {
    min-height: 24px;
    justify-content: space-between;
    color: var(--text-muted);
    font-size: 10px;
  }
  .carousel-buttons { gap: 2px; }
  .project-change {
    gap: 8px;
    color: var(--text-muted);
  }
  .project-change > div {
    display: flex;
    min-width: 0;
    flex: 1;
    flex-direction: column;
    gap: 2px;
  }
  .project-change span,
  .session-folder span {
    color: var(--text-muted);
    font-size: 9px;
    text-transform: uppercase;
  }
  .project-change strong {
    overflow: hidden;
    color: var(--text-secondary);
    font-size: 11px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .session-copy {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-height: 70px;
    padding: 9px 10px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }
  .session-copy strong {
    color: var(--text-primary);
    font-size: 11px;
  }
  .session-copy p {
    display: -webkit-box;
    margin: 0;
    overflow: hidden;
    color: var(--text-secondary);
    font-size: 11px;
    line-height: 1.45;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 3;
    line-clamp: 3;
  }
  .session-copy span {
    margin-top: auto;
    color: var(--text-muted);
    font-size: 9px;
  }
  .preview-status {
    min-height: 70px;
    margin: 0;
    padding: 9px 10px;
    color: var(--text-muted);
    font-size: 11px;
  }
  .error-text { color: var(--accent-red); }
  .session-folder {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: 4px;
  }
  .session-folder code {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    overflow-wrap: anywhere;
  }
</style>
