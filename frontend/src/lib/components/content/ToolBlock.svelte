<!-- ABOUTME: Renders a collapsible tool call block with metadata tags and content. -->
<!-- ABOUTME: Supports Task tool calls with inline subagent conversation expansion. -->
<script lang="ts">
  import type { ToolCall } from "../../api/types.js";
  import SubagentInline from "./SubagentInline.svelte";
  import {
    extractToolParamMeta,
    generateFallbackContent,
  } from "../../utils/tool-params.js";



  interface Props {
    content: string;
    label?: string;
    toolCall?: ToolCall;
  }

  let { content, label, toolCall }: Props = $props();
  let collapsed: boolean = $state(true);
  let outputCollapsed: boolean = $state(true);

  let outputPreviewLine = $derived.by(() => {
    const rc = toolCall?.result_content;
    if (!rc) return "";
    const nl = rc.indexOf("\n");
    return (nl === -1 ? rc : rc.slice(0, nl)).slice(0, 100);
  });

  /** Parsed input parameters from structured tool call data */
  let inputParams = $derived.by(() => {
    if (!toolCall?.input_json) return null;
    try {
      return JSON.parse(toolCall.input_json);
    } catch {
      return null;
    }
  });

  let previewLine = $derived.by(() => {
    const line = content.split("\n")[0]?.slice(0, 100) ?? "";
    if (line) return line;
    // For Edit/Write/Read with no content, show file path as preview
    const filePath =
      inputParams?.file_path ?? inputParams?.path ?? inputParams?.filePath;
    if (filePath) return String(filePath).slice(0, 100);
    // For glob/search tools, show pattern
    if (inputParams?.pattern) return String(inputParams.pattern).slice(0, 100);
    return "";
  });

  /** For Task tool calls, extract key metadata fields */
  let taskMeta = $derived.by(() => {
    if (!isTask || !inputParams)
      return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.subagent_type) {
      meta.push({
        label: "type",
        value: inputParams.subagent_type,
      });
    }
    if (inputParams.description) {
      meta.push({
        label: "description",
        value: inputParams.description,
      });
    }
    return meta.length ? meta : null;
  });

  /** For TaskCreate, show subject and description */
  let taskCreateMeta = $derived.by(() => {
    if (toolCall?.tool_name !== "TaskCreate" || !inputParams)
      return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.subject) {
      meta.push({ label: "subject", value: inputParams.subject });
    }
    if (inputParams.description) {
      meta.push({ label: "description", value: inputParams.description });
    }
    return meta.length ? meta : null;
  });

  /** For TaskUpdate, show taskId and status */
  let taskUpdateMeta = $derived.by(() => {
    if (toolCall?.tool_name !== "TaskUpdate" || !inputParams)
      return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.taskId) {
      meta.push({ label: "task", value: `#${inputParams.taskId}` });
    }
    if (inputParams.status) {
      meta.push({ label: "status", value: inputParams.status });
    }
    if (inputParams.subject) {
      meta.push({ label: "subject", value: inputParams.subject });
    }
    return meta.length ? meta : null;
  });

  /** Extract metadata tags for common tool types */
  let toolParamMeta = $derived.by(() => {
    if (!inputParams || !toolCall) return null;
    const cat = toolCall.category || null;
    const result = cat ? extractToolParamMeta(cat, inputParams) : null;
    return result ?? extractToolParamMeta(toolCall.tool_name, inputParams);
  });

  /** Combined metadata for any tool type */
  let metaTags = $derived(
    taskMeta ??
      taskCreateMeta ??
      taskUpdateMeta ??
      toolParamMeta ??
      null,
  );

  /** Generate content from input_json when regex content is empty.
   *  Try category first (e.g. "Edit"), then fall back to raw tool_name
   *  (e.g. "apply_patch") so tools that don't match their category's
   *  specific field patterns still get the generic key-value output. */
  let fallbackContent = $derived.by(() => {
    if (content || !inputParams || !toolCall) return null;
    const cat = toolCall.category || null;
    const result = cat ? generateFallbackContent(cat, inputParams) : null;
    return result ?? generateFallbackContent(toolCall.tool_name, inputParams);
  });

  let isTask = $derived(
    toolCall?.tool_name === "Task" ||
      toolCall?.tool_name === "Agent" ||
      toolCall?.category === "Task",
  );

  let taskPrompt = $derived(
    isTask ? inputParams?.prompt ?? null : null,
  );

  let subagentSessionId = $derived(
    isTask ? toolCall?.subagent_session_id ?? null : null,
  );
</script>

<div class="tool-block">
  <button
    class="tool-header"
    onclick={() => {
      const sel = window.getSelection();
      if (sel && sel.toString().length > 0) return;
      collapsed = !collapsed;
    }}
  >
    <span class="tool-chevron" class:open={!collapsed}>
      &#9656;
    </span>
    {#if label}
      <span class="tool-label">{label}</span>
    {/if}
    {#if collapsed && previewLine}
      <span class="tool-preview">{previewLine}</span>
    {/if}
  </button>
  {#if !collapsed}
    {#if metaTags}
      <div class="tool-meta">
        {#each metaTags as { label: metaLabel, value }}
          <span class="meta-tag">
            <span class="meta-label">{metaLabel}:</span>
            {value}
          </span>
        {/each}
      </div>
    {/if}
    {#if taskPrompt}
      <pre class="tool-content">{taskPrompt}</pre>
    {:else if content}
      <pre class="tool-content">{content}</pre>
    {:else if fallbackContent}
      <pre class="tool-content">{fallbackContent}</pre>
    {/if}
    {#if toolCall?.result_content}
      <button
        class="output-header"
        onclick={(e) => {
          e.stopPropagation();
          const sel = window.getSelection();
          if (sel && sel.toString().length > 0) return;
          outputCollapsed = !outputCollapsed;
        }}
      >
        <span class="tool-chevron" class:open={!outputCollapsed}>
          &#9656;
        </span>
        <span class="output-label">output</span>
        {#if outputCollapsed && outputPreviewLine}
          <span class="tool-preview">{outputPreviewLine}</span>
        {/if}
      </button>
      {#if !outputCollapsed}
        <pre class="tool-content output-content">{toolCall.result_content}</pre>
      {/if}
    {/if}
  {/if}
  {#if subagentSessionId}
    <SubagentInline sessionId={subagentSessionId} />
  {/if}
</div>

<style>
  .tool-block {
    border-left: 2px solid var(--accent-amber);
    background: var(--tool-bg);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 0;
  }

  .tool-header {
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

  .tool-header:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .tool-chevron {
    display: inline-block;
    font-size: 10px;
    transition: transform 0.15s;
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .tool-chevron.open {
    transform: rotate(90deg);
  }

  .tool-label {
    font-family: var(--font-mono);
    font-weight: 500;
    font-size: 11px;
    color: var(--accent-amber);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .tool-preview {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
  }

  .tool-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding: 6px 14px;
    border-top: 1px solid var(--border-muted);
  }

  .meta-tag {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-muted);
    background: var(--bg-inset);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
  }

  .meta-label {
    color: var(--text-secondary);
    font-weight: 500;
  }

  .tool-content {
    padding: 8px 14px 10px;
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.5;
    overflow-x: auto;
    border-top: 1px solid var(--border-muted);
  }

  .output-header {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 5px 10px;
    width: 100%;
    text-align: left;
    font-size: 12px;
    color: var(--text-secondary);
    min-width: 0;
    border-top: 1px solid var(--border-muted);
    transition: background 0.1s;
    user-select: text;
  }

  .output-header:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .output-label {
    font-family: var(--font-mono);
    font-weight: 500;
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .output-content {
    max-height: 300px;
    overflow-y: auto;
  }
</style>
