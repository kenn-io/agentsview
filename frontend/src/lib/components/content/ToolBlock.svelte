<!-- ABOUTME: Renders a collapsible tool call block with metadata tags and content. -->
<!-- ABOUTME: Supports Task tool calls with inline subagent conversation expansion. -->
<script lang="ts">
  import { onDestroy } from "svelte";
  import type { ToolCall } from "../../api/types.js";
  import SubagentInline from "./SubagentInline.svelte";
  import {
    extractToolParamMeta,
    generateFallbackContent,
  } from "../../utils/tool-params.js";
  import { m } from "../../i18n/index.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { applyHighlight, escapeHTML } from "../../utils/highlight.js";
  import { ChevronRightIcon } from "../../icons.js";
  import { summarizeToolCall } from "../../utils/tool-summary.js";
  import { CopyButton } from "@kenn-io/kit-ui";

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

  type Params = Record<string, unknown>;

  const INTERNAL_COPY_PARAMS = new Set(["agent__intent", "_i"]);

  function stringifyCopyValue(value: unknown): string {
    return typeof value === "string" ? value : JSON.stringify(value);
  }

  function copyParamLines(
    params: Params,
    excluded = new Set<string>(),
  ): string[] {
    const lines: string[] = [];
    for (const [key, value] of Object.entries(params)) {
      if (INTERNAL_COPY_PARAMS.has(key) || excluded.has(key)) continue;
      if (value == null || value === "") continue;
      lines.push(`${key}: ${stringifyCopyValue(value)}`);
    }
    return lines;
  }

  function generateInputCopyContent(
    toolName: string,
    params: Params,
  ): string | null {
    if (toolName === "Task" || toolName === "Agent") return null;
    if (toolName === "Bash" || toolName === "run_command") {
      const cmd = params.command ?? params.cmd;
      if (cmd != null) {
        const lines: string[] = [];
        if (params.description)
          lines.push(`description: ${String(params.description)}`);
        lines.push(`command: ${String(cmd)}`);
        lines.push(
          ...copyParamLines(
            params,
            new Set(["description", "command", "cmd"]),
          ),
        );
        return lines.join("\n");
      }
    }

    const isEdit =
      toolName === "Edit" ||
      params.command === "strReplace";
    if (isEdit) {
      const oldStr =
        params.old_string ?? params.old_str ?? params.oldString ?? params.oldStr;
      const newStr =
        params.new_string ?? params.new_str ?? params.newString ?? params.newStr;
      const diffText = params.diff;
      if (typeof diffText === "string" && diffText) return diffText;
      const patchText = params.patch ?? params.patch_text ?? params.patchText;
      if (typeof patchText === "string" && patchText) return patchText;
      if (oldStr != null || newStr != null) {
        const oldText = String(oldStr ?? "");
        const newText = String(newStr ?? "");
        const oldLines = oldText.split("\n");
        const newLines = newText.split("\n");
        const lines = [`@@ -1,${oldLines.length} +1,${newLines.length} @@`];
        for (const line of oldLines) lines.push(`-${line}`);
        for (const line of newLines) lines.push(`+${line}`);
        return lines.join("\n");
      }
    }

    if (
      toolName === "Write" ||
      (toolName === "write" && params.command === "create")
    ) {
      if (params.content != null) {
        const text = String(params.content);
        if (!text) return "(empty file)";
        const lines = text.split("\n");
        return `@@ -0,0 +1,${lines.length} @@\n${lines.map(line => `+${line}`).join("\n")}`;
      }
    }

    const lines = copyParamLines(params);
    return lines.length ? lines.join("\n") : null;
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
  let inputCopied: boolean = $state(false);
  let outputCopied: boolean = $state(false);
  let inputCopyTimer: ReturnType<typeof setTimeout> | undefined;
  let outputCopyTimer: ReturnType<typeof setTimeout> | undefined;

  // Auto-expand when a search match exists in input or output
  // content. Only reset user overrides when the query itself
  // changes, not when content updates (e.g. during streaming).
  $effect(() => {
    const hq = highlightQuery;
    if (!hq.trim()) {
      searchExpandedInput = false;
      searchExpandedOutput = false;
      contentFullyExpanded = false;
      prevQuery = hq;
      return;
    }
    const q = hq.toLowerCase();
    const inputText = (
      taskPrompt ?? fallbackContent ?? content ?? ""
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
    if (searchExpandedInput) contentFullyExpanded = true;
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

  /** Structured one-line summary from input_json/result_content (ungated). */
  let structuredSummary = $derived(
    toolCall ? summarizeToolCall(toolCall) : null,
  );

  /** Legacy fallback: first line of display content, shown collapsed-only
   *  when no structured summary is available. */
  let legacyPreview = $derived(content.split("\n")[0]?.slice(0, 100) ?? "");

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
  let inputCopyFallback = $derived.by(() => {
    if (content || !inputParams || !toolCall) return null;
    const cat = toolCall.category || null;
    const result = cat ? generateInputCopyContent(cat, inputParams) : null;
    return result ?? generateInputCopyContent(toolCall.tool_name, inputParams);
  });
  let inputCopySource = $derived(taskPrompt ?? inputCopyFallback ?? content ?? "");

  let subagentSessionId = $derived(
    isTask ? toolCall?.subagent_session_id ?? null : null,
  );
  const CONTENT_PREVIEW_LINES = 20;
  let contentFullyExpanded: boolean = $state(false);

  let displayContent = $derived.by(() => {
    const raw = fallbackContent ?? content ?? "";
    if (!raw) return { text: "", isLong: false };
    const lines = raw.split("\n");
    const isLong = lines.length > CONTENT_PREVIEW_LINES;
    if (isLong && !contentFullyExpanded) {
      return {
        text: lines.slice(0, CONTENT_PREVIEW_LINES).join("\n"),
        isLong: true,
        totalLines: lines.length,
      };
    }
    return { text: raw, isLong, totalLines: lines.length };
  });
  let showAllLinesLabel = $derived.by(() => {
    if (!displayContent.isLong) return "";
    return m.tool_block_show_all_lines({
      count: displayContent.totalLines ?? 0,
    });
  });

  let isDiff = $derived.by(() => {
    const text = fallbackContent ?? content ?? "";
    return text.startsWith("--- a/") || text.startsWith("@@");
  });

  let diffLines = $derived.by(() => {
    if (!isDiff) return [];
    const raw = fallbackContent ?? content ?? "";
    return raw.split("\n");
  });

  async function handleInputCopy(event: MouseEvent) {
    event.stopPropagation();
    if (!inputCopySource) return;
    const ok = await copyToClipboard(inputCopySource);
    if (!ok) return;

    clearTimeout(inputCopyTimer);
    inputCopied = true;
    inputCopyTimer = setTimeout(() => {
      inputCopied = false;
    }, 1500);
  }

  async function handleOutputCopy(event: MouseEvent) {
    event.stopPropagation();
    const output = toolCall?.result_content ?? "";
    if (!output) return;
    const ok = await copyToClipboard(output);
    if (!ok) return;

    clearTimeout(outputCopyTimer);
    outputCopied = true;
    outputCopyTimer = setTimeout(() => {
      outputCopied = false;
    }, 1500);
  }

  onDestroy(() => {
    clearTimeout(inputCopyTimer);
    clearTimeout(outputCopyTimer);
  });
</script>

<div class="tool-block" class:in-group={inGroup}>
  <div class="tool-header-row">
    <button
      class="tool-header"
      onclick={() => {
        const sel = window.getSelection();
        if (sel && sel.toString().length > 0) return;
        userCollapsed = !userCollapsed;
        userOverride = true;
        if (userCollapsed) contentFullyExpanded = false;
      }}
    >
      <span class="tool-chevron" class:open={!collapsed}>
        <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
      </span>
      {#if label}
        <span class="tool-label">{label}</span>
      {/if}
      {#if structuredSummary}
        <span class="tool-preview">{structuredSummary}</span>
      {:else if collapsed && legacyPreview}
        <span class="tool-preview">{legacyPreview}</span>
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
    {#if inputCopySource}
      <CopyButton
        class="tool-copy input-copy"
        revealOnHover
        copied={inputCopied}
        ariaLabel={m.tool_block_copy_input()}
        copiedAriaLabel={m.tool_block_copied_input()}
        title={m.tool_block_copy_input()}
        copiedTitle={m.tool_block_copied_input()}
        onclick={handleInputCopy}
      />
    {/if}
  </div>
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
    {:else if fallbackContent && isDiff}
      <div class="diff-view">
        {#each diffLines as line}
          <div class="diff-line {line.startsWith('@@') ? 'diff-hunk' : line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-del' : 'diff-ctx'}">{line}</div>
        {/each}
      </div>
    {:else if displayContent.text}
      <pre class="tool-content" use:applyHighlight={{ q: highlightQuery, current: isCurrentHighlight, content: displayContent.text }}>{@html escapeHTML(displayContent.text)}</pre>
      {#if displayContent.isLong}
        <button
          class="show-more-btn"
          onclick={(e) => {
            e.stopPropagation();
            contentFullyExpanded = !contentFullyExpanded;
          }}
        >
          {contentFullyExpanded
            ? m.tool_block_show_less()
            : showAllLinesLabel}
        </button>
      {/if}
    {/if}
    {#if toolCall?.result_content}
      <div class="output-header-row">
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
            <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
          </span>
          <span class="output-label">{m.tool_block_output()}</span>
          {#if outputCollapsed && outputPreviewLine}
            <span class="tool-preview">{outputPreviewLine}</span>
          {/if}
        </button>
        {#if toolCall.result_content}
          <CopyButton
            class="tool-copy output-copy"
            revealOnHover
            copied={outputCopied}
            ariaLabel={m.tool_block_copy_output()}
            copiedAriaLabel={m.tool_block_copied_output()}
            title={m.tool_block_copy_output()}
            copiedTitle={m.tool_block_copied_output()}
            onclick={handleOutputCopy}
          />
        {/if}
      </div>
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
          <ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" />
        </span>
        <span class="output-label">{m.tool_block_history()}</span>
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
                  <span class="meta-label">{m.tool_block_status_label()}</span>
                  {event.status}
                </span>
                <span class="meta-tag">
                  <span class="meta-label">{m.tool_block_source_label()}</span>
                  {event.source}
                </span>
                {#if event.agent_id}
                  <span class="meta-tag">
                    <span class="meta-label">{m.tool_block_agent_label()}</span>
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

  .tool-header-row,
  .output-header-row {
    display: flex;
    align-items: center;
    min-width: 0;
  }

  .output-header-row {
    border-top: 1px solid var(--border-muted);
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
    flex: 1 1 auto;
    min-width: 0;
  }

  .tool-header:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .tool-chevron {
    display: inline-flex;
    align-items: center;
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
    background: color-mix(in srgb, var(--text-primary) 4%, transparent);
    border: 1px solid color-mix(in srgb, var(--text-primary) 4%, transparent);
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

  .show-more-btn {
    display: block;
    width: 100%;
    padding: 4px 14px;
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--accent-blue, #58a6ff);
    text-align: left;
    border-top: 1px solid var(--border-muted);
    transition: background 0.1s;
  }

  .show-more-btn:hover {
    background: var(--bg-surface-hover);
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
    transition: background 0.1s;
    user-select: text;
    flex: 1 1 auto;
    min-width: 0;
  }

  .output-header:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  :global(.tool-copy.kit-copy-btn) {
    flex: 0 0 auto;
    margin-right: 8px;
  }

  .tool-block:hover :global(.tool-copy.kit-copy-btn),
  .tool-header-row:focus-within :global(.tool-copy.kit-copy-btn),
  .output-header-row:focus-within :global(.tool-copy.kit-copy-btn) {
    opacity: 1;
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
