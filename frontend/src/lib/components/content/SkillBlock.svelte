<script lang="ts">
  import { renderMarkdown } from "../../utils/markdown.js";
  import { highlightCodeFences } from "../../utils/highlight-fences.js";

  interface Props {
    content: string;
    name?: string;
  }

  let { content, name }: Props = $props();
  let collapsed: boolean = $state(true);

  let previewLine = $derived(
    content.split("\n")[0]?.slice(0, 80) ?? "",
  );
</script>

<div class="skill-block">
  <button
    class="skill-header"
    onclick={() => {
      const sel = window.getSelection();
      if (sel && sel.toString().length > 0) return;
      collapsed = !collapsed;
    }}
  >
    <span class="skill-chevron" class:open={!collapsed}>
      &#9656;
    </span>
    <span class="skill-label">Skill: {name ?? "unknown"}</span>
    {#if collapsed && previewLine}
      <span class="skill-preview">{previewLine}</span>
    {/if}
  </button>
  {#if !collapsed}
    <div
      class="skill-content markdown"
      use:highlightCodeFences={{ content }}
    >
      {@html renderMarkdown(content)}
    </div>
  {/if}
</div>

<style>
  .skill-block {
    border-left: 2px solid var(--accent-teal, #14b8a6);
    background: var(--tool-bg);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 0;
  }

  .skill-header {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    width: 100%;
    text-align: left;
    font-size: 12px;
    color: var(--text-secondary);
    min-width: 0;
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    transition: background 0.1s;
    user-select: text;
  }

  .skill-header:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .skill-chevron {
    display: inline-block;
    font-size: 10px;
    transition: transform 0.15s;
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .skill-chevron.open {
    transform: rotate(90deg);
  }

  .skill-label {
    font-family: var(--font-mono);
    font-weight: 500;
    font-size: 11px;
    color: var(--accent-teal, #14b8a6);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .skill-preview {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
  }

  .skill-content {
    padding: 8px 14px 12px;
    font-size: 13px;
    color: var(--text-secondary);
    line-height: 1.65;
    border-top: 1px solid var(--border-muted);
    overflow-x: auto;
  }
</style>
