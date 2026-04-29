<!-- ABOUTME: Renders a collapsible tool call block with metadata tags and content. -->
<!-- ABOUTME: Supports Task tool calls with inline subagent conversation expansion. -->
<script lang="ts">
  import type { ToolCall } from "../../api/types.js";
  import SubagentInline from "./SubagentInline.svelte";
  import {
    extractToolParamMeta,
    generateFallbackContent,
  } from "../../utils/tool-params.js";
  import { applyHighlight, escapeHTML } from "../../utils/highlight.js";

  interface Props {
    content: string;
    label?: string;
    toolCall?: ToolCall;
    highlightQuery?: string;
    isCurrentHighlight?: boolean;
    /** Pre-formatted duration label (e.g. "2.4s", "running 1m 28s+"). Null/undefined renders no badge. */
    durationLabel?: string;
    /** Tints the duration badge with the slow color family. */
    isSlow?: boolean;
    /** Tints the duration badge green and pulses it. */
    isRunning?: boolean;
    /** When true, the block sits inside a ParallelGroup — flatten outer margin and corner radii. */
    inGroup?: boolean;
  }

  let {
    content,
    label,
    toolCall,
    highlightQuery = "",
    isCurrentHighlight = false,
    durationLabel,
    isSlow = false,
    isRunning = false,
    inGroup = false,
  }: Props = $props();
  let userCollapsed: boolean = $state(true);
  let userOutputCollapsed: boolean = $state(true);
  let userHistoryCollapsed: boolean = $state(true);
  let userOverride: boolean = $state(false);
  let userOutputOverride: boolean = $state(false);
  let userHistoryOverride: boolean = $state(false);
  let searchExpandedInput: boolean = $state(false);
  let searchExpandedOutput: boolean = $state(false);
  let searchExpandedHistory: boolean = $state(false);
  let prevQuery: string = "";

  // Auto-expand when a search match exists in input or output
  // content. Only reset user overrides when the query itself
  // changes, not when content updates (e.g. during streaming).
  $effect(() => {
    const hq = highlightQuery;
    if (!hq.trim()) {
      searchExpandedInput = false;
      searchExpandedOutput = false;
      prevQuery = hq;
      return;
    }
    const q = hq.toLowerCase();
    const inputText = (
      taskPrompt ?? content ?? fallbackContent ?? ""
    ).toLowerCase();
    const historyText = (
      toolCall?.result_events?.map((event) => event.content).join("\n\n") ?? ""
    ).toLowerCase();
    const outputText = (
      [toolCall?.result_content ?? "", historyText].filter(Boolean).join("\n\n")
    ).toLowerCase();
    searchExpandedInput = inputText.includes(q);
    searchExpandedOutput = outputText.includes(q);
    searchExpandedHistory = historyText.includes(q);
    if (hq !== prevQuery) {
      userOverride = false;
      userOutputOverride = false;
      userHistoryOverride = false;
      prevQuery = hq;
    }
  });

  let collapsed = $derived(
    userOverride ? userCollapsed
      : (searchExpandedInput || searchExpandedOutput) ? false
      : userCollapsed,
  );
  let outputCollapsed = $derived(
    userOutputOverride ? userOutputCollapsed
      : searchExpandedOutput ? false
      : userOutputCollapsed,
  );
  let historyCollapsed = $derived(
    userHistoryOverride ? userHistoryCollapsed
      : searchExpandedHistory ? false
      : userHistoryCollapsed,
  );

  let outputPreviewLine = $derived.by(() => {
    const rc = toolCall?.result_content;
    if (!rc) return "";
    const nl = rc.indexOf("\n");
    return (nl === -1 ? rc : rc.slice(0, nl)).slice(0, 100);
  });

  let resultEvents = $derived(toolCall?.result_events ?? []);

  let historyPreviewLine = $derived.by(() => {
    const last = resultEvents[resultEvents.length - 1];
    if (!last) return "";
    return `${last.status}: ${last.content.split("\n")[0]}`.slice(0, 100);
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
    // For Bash tools, surface the command in the collapsed header so
    // codex exec_command (cmd) and Claude Bash (command) are both
    // legible without expanding the block.
    const cmd = inputParams?.command ?? inputParams?.cmd;
    if (cmd) {
      const firstLine = String(cmd).split("\n")[0] ?? "";
      return `$ ${firstLine}`.slice(0, 100);
    }
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
    return extractToolParamMeta(toolCall.tool_name, inputParams, toolCall.category);
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
      toolCall?.category === "Task" ||
      (toolCall?.tool_name?.includes("subagent") ?? false),
  );

  let taskPrompt = $derived(
    isTask ? inputParams?.prompt ?? null : null,
  );

  let subagentSessionId = $derived(
    isTask ? toolCall?.subagent_session_id ?? null : null,
  );
  let isDiff = $derived.by(() => {
    const text = fallbackContent ?? content ?? "";
    return text.startsWith("--- a/") || text.startsWith("@@");
  });

  let diffLines = $derived.by(() => {
    if (!isDiff) return [];
    const raw = fallbackContent ?? content ?? "";
    return raw.split("\n");
  });
</script>

<div class="tool-block" class:in-group={inGroup}>
  <button
    class="tool-header"
    onclick={() => {
      const sel = window.getSelection();
      if (sel && sel.toString().length > 0) return;
      userCollapsed = !userCollapsed;
      userOverride = true;
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
    {#if durationLabel}
      <span
        class="tool-duration"
        class:slow={isSlow}
        class:running={isRunning}
      >
        {durationLabel}
      </span>
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
      <pre class="tool-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content: taskPrompt }}>{@html escapeHTML(taskPrompt)}</pre>
    {:else if content}
      <pre class="tool-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content }}>{@html escapeHTML(content)}</pre>
    {:else if fallbackContent && isDiff}
      <div class="diff-view">
        {#each diffLines as line}
          <div class="diff-line {line.startsWith('@@') ? 'diff-hunk' : line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-del' : 'diff-ctx'}">{line}</div>
        {/each}
      </div>
    {:else if fallbackContent}
      <pre class="tool-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content: fallbackContent }}>{@html escapeHTML(fallbackContent)}</pre>
    {/if}
    {#if toolCall?.result_content}
      <button
        class="output-header"
        onclick={(e) => {
          e.stopPropagation();
          const sel = window.getSelection();
          if (sel && sel.toString().length > 0) return;
          userOutputCollapsed = !userOutputCollapsed;
          userOutputOverride = true;
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
        <pre class="tool-content output-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content: toolCall.result_content }}>{@html escapeHTML(toolCall.result_content)}</pre>
      {/if}
    {/if}
    {#if resultEvents.length > 0}
      <button
        class="history-header"
        onclick={(e) => {
          e.stopPropagation();
          const sel = window.getSelection();
          if (sel && sel.toString().length > 0) return;
          userHistoryCollapsed = !userHistoryCollapsed;
          userHistoryOverride = true;
        }}
      >
        <span class="tool-chevron" class:open={!historyCollapsed}>
          &#9656;
        </span>
        <span class="output-label">history</span>
        {#if historyCollapsed && historyPreviewLine}
          <span class="tool-preview">{historyPreviewLine}</span>
        {/if}
      </button>
      {#if !historyCollapsed}
        <div class="result-history">
          {#each resultEvents as event (event.event_index)}
            <div class="result-event">
              <div class="result-event-meta">
                <span class="meta-tag">
                  <span class="meta-label">status:</span>
                  {event.status}
                </span>
                <span class="meta-tag">
                  <span class="meta-label">source:</span>
                  {event.source}
                </span>
                {#if event.agent_id}
                  <span class="meta-tag">
                    <span class="meta-label">agent:</span>
                    {event.agent_id}
                  </span>
                {/if}
              </div>
              <pre class="tool-content output-content history-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content: event.content }}>{@html escapeHTML(event.content)}</pre>
            </div>
          {/each}
        </div>
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

  .tool-block.in-group {
    margin: 0;
    border-left: none;
    border-radius: 0;
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

  .tool-duration {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    padding: 2px 7px;
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.04);
    border-radius: var(--radius-sm);
    flex-shrink: 0;
    margin-left: auto;
  }

  .tool-duration.slow {
    color: var(--slow-fg);
    background: var(--slow-bg);
    border-color: var(--slow-ring);
  }

  .tool-duration.running {
    color: var(--running-fg);
    background: var(--running-bg);
    border-color: var(--running-ring);
    animation: duration-pulse 1.6s ease-in-out infinite;
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

  .history-header {
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

  .history-header:hover {
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

  .result-history {
    border-top: 1px solid var(--border-muted);
  }

  .result-event + .result-event {
    border-top: 1px solid var(--border-muted);
  }

  .result-event-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding: 6px 14px 0;
  }

  .history-content {
    border-top: 0;
    margin-top: 0;
  }

  .diff-view {
    font-family: var(--font-mono);
    font-size: 12px;
    line-height: 1.5;
    overflow-x: auto;
    border-top: 1px solid var(--border-muted);
    padding: 4px 0;
    max-height: 400px;
    overflow-y: auto;
  }

  .diff-line {
    padding: 0 14px;
    white-space: pre;
  }

  .diff-hunk {
    color: var(--accent-blue, #58a6ff);
    background: color-mix(in srgb, var(--accent-blue, #58a6ff) 8%, transparent);
    padding: 2px 14px;
    margin: 2px 0;
  }

  .diff-add {
    color: var(--accent-green, #3fb950);
    background: color-mix(in srgb, var(--accent-green, #3fb950) 10%, transparent);
  }

  .diff-del {
    color: var(--accent-red, #f85149);
    background: color-mix(in srgb, var(--accent-red, #f85149) 10%, transparent);
  }

  .diff-ctx {
    color: var(--text-muted);
  }
</style>
