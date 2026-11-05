<script lang="ts">
  import { m } from "../../i18n/index.js";
  import {
    ArrowDownToLineIcon,
    ArrowDownWideNarrowIcon,
    ArrowUpNarrowWideIcon,
  } from "../../icons.js";
  import { ui } from "../../stores/ui.svelte.js";
  import TranscriptViewMenu from "./TranscriptViewMenu.svelte";

  // Controls that act on the open transcript: what it shows, whether it
  // follows new messages, and its order. They sit beside the session title
  // on wide screens and in the top bar on narrow ones.
</script>

<span class="transcript-controls">
  <TranscriptViewMenu />
  <!-- Follow latest stays visible: it is live state, and its pressed look
       is the only sign that the view is pinned to new messages. -->
  <button
    class="control-btn"
    class:active={ui.followLatest}
    onclick={() => ui.toggleFollowLatest()}
    title={m.header_actions_follow_latest()}
    aria-label={m.header_actions_follow_latest()}
    aria-pressed={ui.followLatest}
  >
    <ArrowDownToLineIcon size="14" aria-hidden="true" />
  </button>
  <button
    class="control-btn"
    onclick={() => ui.toggleSort()}
    title={m.header_actions_toggle_sort()}
    aria-label={m.header_actions_toggle_sort()}
  >
    {#if ui.sortNewestFirst}
      <ArrowDownWideNarrowIcon size="14" aria-hidden="true" />
    {:else}
      <ArrowUpNarrowWideIcon size="14" aria-hidden="true" />
    {/if}
  </button>
</span>

<style>
  .transcript-controls {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    flex-shrink: 0;
  }

  .control-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    flex-shrink: 0;
    transition: background 0.12s, color 0.12s;
  }

  .control-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .control-btn.active {
    background: var(--selected-bg);
    box-shadow: var(--selected-edge);
    color: var(--text-primary);
  }

  @media (pointer: coarse) {
    .control-btn {
      width: 44px;
      height: 44px;
    }
  }
</style>
