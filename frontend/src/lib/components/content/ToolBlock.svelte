<!-- ABOUTME: Renders a collapsible tool call block with metadata tags and content. -->
<!-- ABOUTME: Supports Task tool calls with inline subagent conversation expansion. -->
<script lang="ts">
  import { onDestroy } from "svelte";
  import type { ToolCall } from "../../api/types.js";
  import SubagentInline from "./SubagentInline.svelte";
  import { extractToolParamMeta, type MetaTag } from "../../utils/tool-params.js";
  import { resolveToolInput } from "../../search/tool-input.js";
  import { searchBlock } from "../../search/session-block.svelte.js";
  import { searchCollapsed, toolSearchKey, type ToolSearchScope } from "../../search/component-state.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import SearchMatchCount from "./SearchMatchCount.svelte";
  import { m } from "../../i18n/index.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { highlightCodeFences } from "../../utils/highlight-fences.js";
  import { ChevronRightIcon } from "../../icons.js";
  import { summarizeToolCall, summarizeToolCallPath } from "../../utils/tool-summary.js";
  import { CopyButton, SegmentedControl, type SegmentedControlOption } from "@kenn-io/kit-ui";
  import { renderMarkdown } from "../../utils/markdown.js";
  import {
    displayFormattedToolResult,
    displayToolResult,
  } from "../../utils/toolDisplay.js";
  import { ui } from "../../stores/ui.svelte.js";

  interface Props {
    content: string;
    label?: string;
    toolCall?: ToolCall;
    searchScope?: ToolSearchScope;
    /** Pre-formatted duration label; undefined hides the badge. */
    durationLabel?: string;
    isSlow?: boolean;
    isRunning?: boolean;
    /** Flatten outer spacing when inside a parallel group. */
    inGroup?: boolean;
  }

  type Params = Record<string, unknown>;
  const INTERNAL_COPY_PARAMS = new Set(["agent__intent", "_i"]);

  function stringifyCopyValue(value: unknown): string {
    return typeof value === "string" ? value : JSON.stringify(value);
  }

  function copyParamLines(params: Params, excluded = new Set<string>()): string[] {
    const lines: string[] = [];
    for (const [key, value] of Object.entries(params)) {
      if (INTERNAL_COPY_PARAMS.has(key) || excluded.has(key)) continue;
      if (value == null || value === "") continue;
      lines.push(`${key}: ${stringifyCopyValue(value)}`);
    }
    return lines;
  }

  function generateInputCopyContent(toolName: string, params: Params): string | null {
    if (toolName === "Task" || toolName === "Agent") return null;
    if (toolName === "Bash" || toolName === "run_command") {
      const cmd = params.command ?? params.cmd;
      if (cmd != null) {
        const lines: string[] = [];
        if (params.description) lines.push(`description: ${String(params.description)}`);
        lines.push(`command: ${String(cmd)}`);
        lines.push(...copyParamLines(params, new Set(["description", "command", "cmd"])));
        return lines.join("\n");
      }
    }
    const isEdit = toolName === "Edit" || params.command === "strReplace";
    if (isEdit) {
      const oldStr = params.old_string ?? params.old_str ?? params.oldString ?? params.oldStr;
      const newStr = params.new_string ?? params.new_str ?? params.newString ?? params.newStr;
      const diffText = params.diff;
      if (typeof diffText === "string" && diffText) return diffText;
      const patchText = params.patch ?? params.patch_text ?? params.patchText;
      if (typeof patchText === "string" && patchText) return patchText;
      if (oldStr != null || newStr != null) {
        const oldLines = String(oldStr ?? "").split("\n");
        const newLines = String(newStr ?? "").split("\n");
        const lines = [`@@ -1,${oldLines.length} +1,${newLines.length} @@`];
        for (const line of oldLines) lines.push(`-${line}`);
        for (const line of newLines) lines.push(`+${line}`);
        return lines.join("\n");
      }
    }
    if (toolName === "Write" || (toolName === "write" && params.command === "create")) {
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

  let { content, label, toolCall, searchScope, durationLabel,
    isSlow = false, isRunning = false, inGroup = false }: Props = $props();
  let userCollapsed = $state(true);
  let userOutputCollapsed = $state(true);
  let userHistoryCollapsed = $state(true);
  let overrideSeq = $state(-1);
  let outputOverrideSeq = $state(-1);
  let historyOverrideSeq = $state(-1);
  let userContentFullyExpanded = $state(false);
  let contentOverrideSeq = $state(-1);
  let inputCopied = $state(false);
  let outputCopied = $state(false);
  let outputMode: "raw" | "formatted" = $state("raw");
  // The index uses source text. Temporarily expose that same representation
  // during find without overwriting the user's formatted-output preference.
  let rawOutputForSearch = $derived(searchScope !== undefined && inSessionSearch.isActive);
  let inputCopyTimer: ReturnType<typeof setTimeout> | undefined;
  let outputCopyTimer: ReturnType<typeof setTimeout> | undefined;

  let inputKey = $derived(toolSearchKey(searchScope, "tool-input"));
  let outputKey = $derived(toolSearchKey(searchScope, "tool-output"));
  let resultEvents = $derived((toolCall?.result_events ?? []).map((event) => ({
    ...event, content: displayToolResult(event.content),
  })));
  let historyKeys = $derived(resultEvents.map((_, index) => toolSearchKey(searchScope, "tool-history", index)));
  let currentInput = $derived(inSessionSearch.isCurrentBlock(inputKey));
  let currentOutput = $derived(inSessionSearch.isCurrentBlock(outputKey));
  let currentHistory = $derived(historyKeys.some((key) => inSessionSearch.isCurrentBlock(key)));
  let historyCount = $derived(historyKeys.reduce((count, key) => count + inSessionSearch.countForBlock(key), 0));
  let matchCount = $derived(inSessionSearch.countForBlock(inputKey) + inSessionSearch.countForBlock(outputKey) + historyCount);
  let collapsed = $derived(searchCollapsed(userCollapsed,
    currentInput || currentOutput || currentHistory, inSessionSearch.navigationRevision, overrideSeq));
  let outputCollapsed = $derived(searchCollapsed(userOutputCollapsed,
    currentOutput, inSessionSearch.navigationRevision, outputOverrideSeq));
  let historyCollapsed = $derived(searchCollapsed(userHistoryCollapsed,
    currentHistory, inSessionSearch.navigationRevision, historyOverrideSeq));
  let contentFullyExpanded = $derived(
    contentOverrideSeq === inSessionSearch.navigationRevision ? userContentFullyExpanded
      : currentInput || userContentFullyExpanded,
  );

  let outputContent = $derived(displayToolResult(toolCall?.result_content ?? ""));
  let formattedOutputContent = $derived(
    displayFormattedToolResult(toolCall?.result_content ?? ""),
  );

  let outputPreviewLine = $derived.by(() => {
    const rc = outputContent;
    if (!rc) return "";
    const nl = rc.indexOf("\n");
    return (nl === -1 ? rc : rc.slice(0, nl)).slice(0, 100);
  });
  let historyPreviewLine = $derived.by(() => {
    const last = resultEvents[resultEvents.length - 1];
    return last ? `${last.status}: ${last.content.split("\n")[0]}`.slice(0, 100) : "";
  });
  let inputParams = $derived.by(() => {
    if (!toolCall?.input_json) return null;
    try { return JSON.parse(toolCall.input_json); } catch { return null; }
  });
  let structuredSummary = $derived(toolCall ? summarizeToolCall(toolCall) : null);
  let structuredSummaryTitle = $derived(toolCall ? summarizeToolCallPath(toolCall) : null);
  const outputModeOptions = $derived<SegmentedControlOption[]>([
    { value: "raw", label: m.tool_block_raw() },
    { value: "formatted", label: m.tool_block_formatted() },
  ]);
  let legacyPreview = $derived(content.split("\n")[0]?.slice(0, 100) ?? "");

  let taskMeta = $derived.by(() => {
    if (!isTask || !inputParams) return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.subagent_type) meta.push({ label: "type", value: inputParams.subagent_type });
    if (inputParams.description) meta.push({ label: "description", value: inputParams.description });
    return meta.length ? meta : null;
  });
  let taskCreateMeta = $derived.by(() => {
    if (toolCall?.tool_name !== "TaskCreate" || !inputParams) return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.subject) meta.push({ label: "subject", value: inputParams.subject });
    if (inputParams.description) meta.push({ label: "description", value: inputParams.description });
    return meta.length ? meta : null;
  });
  let taskUpdateMeta = $derived.by(() => {
    if (toolCall?.tool_name !== "TaskUpdate" || !inputParams) return null;
    const meta: { label: string; value: string }[] = [];
    if (inputParams.taskId) meta.push({ label: "task", value: `#${inputParams.taskId}` });
    if (inputParams.status) meta.push({ label: "status", value: inputParams.status });
    if (inputParams.subject) meta.push({ label: "subject", value: inputParams.subject });
    return meta.length ? meta : null;
  });
  let toolParamMeta = $derived.by(() => {
    if (!inputParams || !toolCall) return null;
    return extractToolParamMeta(toolCall.tool_name, inputParams, toolCall.category);
  });
  let metaTags = $derived<MetaTag[] | null>(taskMeta ?? taskCreateMeta ?? taskUpdateMeta ?? toolParamMeta ?? null);

  // Display and search share this source; copy retains the complete payload.
  let resolvedInput = $derived(resolveToolInput(toolCall, content));
  let fallbackContent = $derived(resolvedInput.fallbackContent);
  let isTask = $derived(resolvedInput.isTask);
  let taskPrompt = $derived(resolvedInput.taskPrompt);
  let inputCopyFallback = $derived.by(() => {
    if (content || !inputParams || !toolCall) return null;
    const cat = toolCall.category || null;
    const result = cat ? generateInputCopyContent(cat, inputParams) : null;
    return result ?? generateInputCopyContent(toolCall.tool_name, inputParams);
  });
  let inputCopySource = $derived(taskPrompt ?? inputCopyFallback ?? content ?? "");
  let subagentSessionId = $derived(isTask ? toolCall?.subagent_session_id ?? null : null);
  const CONTENT_PREVIEW_LINES = 20;
  let displayContent = $derived.by(() => {
    const raw = resolvedInput.text;
    if (!raw) return { text: "", isLong: false };
    const lines = raw.split("\n");
    const isLong = lines.length > CONTENT_PREVIEW_LINES;
    return { text: isLong && !contentFullyExpanded ? lines.slice(0, CONTENT_PREVIEW_LINES).join("\n") : raw,
      isLong, totalLines: lines.length };
  });
  let showAllLinesLabel = $derived(displayContent.isLong
    ? m.tool_block_show_all_lines({ count: displayContent.totalLines ?? 0 }) : "");
  let isDiff = $derived(resolvedInput.text.startsWith("--- a/") || resolvedInput.text.startsWith("@@"));
  let diffLines = $derived(isDiff ? resolvedInput.text.split("\n") : []);

  async function handleInputCopy(event: MouseEvent) {
    event.stopPropagation();
    if (!inputCopySource) return;
    const ok = await copyToClipboard(inputCopySource);
    if (!ok) return;
    clearTimeout(inputCopyTimer);
    inputCopied = true;
    inputCopyTimer = setTimeout(() => { inputCopied = false; }, 1500);
  }
  async function handleOutputCopy(event: MouseEvent) {
    event.stopPropagation();
    const output = toolCall?.result_content ?? "";
    if (!output) return;
    const ok = await copyToClipboard(output);
    if (!ok) return;
    clearTimeout(outputCopyTimer);
    outputCopied = true;
    outputCopyTimer = setTimeout(() => { outputCopied = false; }, 1500);
  }
  onDestroy(() => { clearTimeout(inputCopyTimer); clearTimeout(outputCopyTimer); });
</script>

<div class="tool-block" class:in-group={inGroup}>
  <div class="tool-header-row">
    <button class="tool-header" aria-expanded={!collapsed} onclick={() => {
      const sel = window.getSelection();
      if (sel && sel.toString().length > 0) return;
      userCollapsed = !collapsed;
      overrideSeq = inSessionSearch.navigationRevision;
      if (userCollapsed) {
        userContentFullyExpanded = false;
        contentOverrideSeq = inSessionSearch.navigationRevision;
      }
    }}>
      <span class="tool-chevron" class:open={!collapsed}><ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" /></span>
      {#if label}<span class="tool-label">{label}</span>{/if}
      <SearchMatchCount count={matchCount} />
      {#if structuredSummary}
        <span class="tool-preview" title={structuredSummaryTitle ?? undefined}>
          {structuredSummary}{#if structuredSummaryTitle}<span class="kit-sr-only">{structuredSummaryTitle}</span>{/if}
        </span>
      {:else if collapsed && legacyPreview}<span class="tool-preview">{legacyPreview}</span>{/if}
      {#if durationLabel}<span class="tool-duration" class:slow={isSlow} class:running={isRunning}>{durationLabel}</span>{/if}
    </button>
    {#if inputCopySource}
      <CopyButton class="tool-copy input-copy" revealOnHover copied={inputCopied}
        ariaLabel={m.tool_block_copy_input()} copiedAriaLabel={m.tool_block_copied_input()}
        title={m.tool_block_copy_input()} copiedTitle={m.tool_block_copied_input()} onclick={handleInputCopy} />
    {/if}
  </div>
  {#if !collapsed}
    {#if metaTags}
      <div class="tool-meta">
        {#each metaTags as { label: metaLabel, value, displayValue }}
          <span class="meta-tag"><span class="meta-label">{metaLabel}:</span>
            {#if displayValue}<span class="meta-value" title={value}>{displayValue}<span class="kit-sr-only">{value}</span></span>
            {:else}<span>{value}</span>{/if}
          </span>
        {/each}
      </div>
    {/if}
    {#if taskPrompt}
      <pre class="tool-content" {@attach searchBlock(inputKey)}>{taskPrompt}</pre>
    {:else if fallbackContent && isDiff}
      <pre class="diff-view" {@attach searchBlock(inputKey)}>{#each diffLines as line, index}<span class="diff-line {line.startsWith('@@') ? 'diff-hunk' : line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-del' : 'diff-ctx'}">{line}</span>{index < diffLines.length - 1 ? "\n" : ""}{/each}</pre>
    {:else if displayContent.text}
      <pre class="tool-content" {@attach searchBlock(inputKey)}>{displayContent.text}</pre>
      {#if displayContent.isLong}
        <button class="show-more-btn" onclick={(e) => {
          e.stopPropagation();
          userContentFullyExpanded = !contentFullyExpanded;
          contentOverrideSeq = inSessionSearch.navigationRevision;
        }}>{contentFullyExpanded ? m.tool_block_show_less() : showAllLinesLabel}</button>
      {/if}
    {/if}
    {#if toolCall?.result_content}
      <div class="output-header-row">
        <button class="output-header" aria-expanded={!outputCollapsed} onclick={(e) => {
          e.stopPropagation();
          const sel = window.getSelection();
          if (sel && sel.toString().length > 0) return;
          userOutputCollapsed = !outputCollapsed;
          outputOverrideSeq = inSessionSearch.navigationRevision;
        }}>
          <span class="tool-chevron" class:open={!outputCollapsed}><ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" /></span>
          <span class="output-label">{m.tool_block_output()}</span>
          <SearchMatchCount searchKey={outputKey} />
          {#if outputCollapsed && outputPreviewLine}<span class="tool-preview">{outputPreviewLine}</span>{/if}
        </button>
        {#if !outputCollapsed && rawOutputForSearch}
          <span class="search-output-hint">{m.session_find_raw_output()}</span>
        {:else if !outputCollapsed}
          <SegmentedControl class="output-mode" options={outputModeOptions} value={outputMode}
            ariaLabel={m.tool_block_output_mode()} onchange={(next) => (outputMode = next as "raw" | "formatted")} />
        {/if}
        <CopyButton class="tool-copy output-copy" revealOnHover copied={outputCopied}
          ariaLabel={m.tool_block_copy_output()} copiedAriaLabel={m.tool_block_copied_output()}
          title={m.tool_block_copy_output()} copiedTitle={m.tool_block_copied_output()} onclick={handleOutputCopy} />
      </div>
      {#if !outputCollapsed}
        {#if outputMode === "formatted" && !rawOutputForSearch}
          <div class="tool-content output-content formatted-output"
            {@attach searchBlock(outputKey)} use:highlightCodeFences={{ content: formattedOutputContent }}>
            {@html renderMarkdown(formattedOutputContent, { renderUnknownXmlBlocksAsPreformatted: ui.renderUnknownXmlBlocksAsPreformatted })}
          </div>
        {:else}
          <pre class="tool-content output-content" {@attach searchBlock(outputKey)}>{outputContent}</pre>
        {/if}
      {/if}
    {/if}
    {#if resultEvents.length > 0}
      <button class="history-header" aria-expanded={!historyCollapsed} onclick={(e) => {
        e.stopPropagation();
        const sel = window.getSelection();
        if (sel && sel.toString().length > 0) return;
        userHistoryCollapsed = !historyCollapsed;
        historyOverrideSeq = inSessionSearch.navigationRevision;
      }}>
        <span class="tool-chevron" class:open={!historyCollapsed}><ChevronRightIcon size="10" strokeWidth="2.4" aria-hidden="true" /></span>
        <span class="output-label">{m.tool_block_history()}</span>
        <SearchMatchCount count={historyCount} />
        {#if historyCollapsed && historyPreviewLine}<span class="tool-preview">{historyPreviewLine}</span>{/if}
      </button>
      {#if !historyCollapsed}
        <div class="result-history">
          {#each resultEvents as event, eventIndex (event.event_index)}
            <div class="result-event">
              <div class="result-event-meta">
                <span class="meta-tag"><span class="meta-label">{m.tool_block_status_label()}</span>{event.status}</span>
                <span class="meta-tag"><span class="meta-label">{m.tool_block_source_label()}</span>{event.source}</span>
                {#if event.agent_id}<span class="meta-tag"><span class="meta-label">{m.tool_block_agent_label()}</span>{event.agent_id}</span>{/if}
              </div>
              <pre class="tool-content output-content history-content" {@attach searchBlock(historyKeys[eventIndex])}>{event.content}</pre>
            </div>
          {/each}
        </div>
      {/if}
    {/if}
  {/if}
  {#if subagentSessionId}<SubagentInline sessionId={subagentSessionId} />{/if}
</div>

<style>
  .search-output-hint { color: var(--text-muted); font-size: 10px; margin-inline-start: auto; }
  .tool-block {
    border-left: 2px solid var(--accent-amber);
    background: var(--tool-bg);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    margin: 0;
  }
  .tool-block.in-group { margin: 0; border-left: none; border-radius: 0; }
  .tool-header-row, .output-header-row { display: flex; align-items: center; min-width: 0; }
  .output-header-row { border-top: 1px solid var(--border-muted); }
  .tool-header {
    display: flex; align-items: center; gap: 6px; padding: 6px 10px; width: 100%;
    text-align: left; font-size: 12px; color: var(--text-secondary); min-width: 0;
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0; transition: background 0.1s;
    user-select: text; flex: 1 1 auto;
  }
  .tool-header:hover { background: var(--bg-surface-hover); color: var(--text-primary); }
  .tool-chevron { display: inline-flex; align-items: center; transition: transform 0.15s; flex-shrink: 0; color: var(--text-muted); }
  .tool-chevron.open { transform: rotate(90deg); }
  .tool-label { font-family: var(--font-mono); font-weight: 500; font-size: 11px; color: var(--accent-amber); white-space: nowrap; flex-shrink: 0; }
  .tool-preview { font-family: var(--font-mono); font-size: 12px; color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; min-width: 0; }
  .tool-duration {
    font-family: var(--font-mono); font-size: 10px; color: var(--text-muted); padding: 2px 7px;
    background: color-mix(in srgb, var(--text-primary) 4%, transparent);
    border: 1px solid color-mix(in srgb, var(--text-primary) 4%, transparent);
    border-radius: var(--radius-sm); flex-shrink: 0; margin-left: auto;
  }
  .tool-duration.slow { color: var(--slow-fg); background: var(--slow-bg); border-color: var(--slow-ring); }
  .tool-duration.running { color: var(--running-fg); background: var(--running-bg); border-color: var(--running-ring); animation: duration-pulse 1.6s ease-in-out infinite; }
  .tool-meta { display: flex; flex-wrap: wrap; gap: 6px; padding: 6px 14px; border-top: 1px solid var(--border-muted); }
  .meta-tag { font-family: var(--font-mono); font-size: 11px; color: var(--text-muted); background: var(--bg-inset); padding: 2px 6px; border-radius: var(--radius-sm); }
  .meta-label { color: var(--text-secondary); font-weight: 500; }
  .show-more-btn {
    display: block; width: 100%; padding: 4px 14px; font-family: var(--font-mono); font-size: 11px;
    color: var(--accent-blue, #58a6ff); text-align: left; border-top: 1px solid var(--border-muted); transition: background 0.1s;
  }
  .show-more-btn:hover { background: var(--bg-surface-hover); }
  .tool-content { padding: 8px 14px 10px; font-family: var(--font-mono); font-size: 12px; color: var(--text-secondary); line-height: 1.5; overflow-x: auto; border-top: 1px solid var(--border-muted); }
  .output-header {
    display: flex; align-items: center; gap: 6px; padding: 5px 10px; width: 100%; text-align: left;
    font-size: 12px; color: var(--text-secondary); min-width: 0; transition: background 0.1s; user-select: text; flex: 1 1 auto;
  }
  :global(.tool-block .output-mode) { flex: 0 0 auto; margin-left: auto; }
  .formatted-output :global(pre) { white-space: pre-wrap; }
  .tool-preview, .meta-value { position: relative; }
  :global(.tool-preview .kit-sr-only), :global(.meta-tag .kit-sr-only) {
    left: 0; top: 0; white-space: normal; overflow-wrap: anywhere; overflow: hidden; text-overflow: ellipsis;
  }
  .formatted-output :global(img) { max-width: 100%; height: auto; }
  .formatted-output :global(p) { overflow-wrap: anywhere; }
  .output-header:hover { background: var(--bg-surface-hover); color: var(--text-primary); }
  :global(.tool-copy.kit-copy-btn) { flex: 0 0 auto; margin-right: 8px; }
  .tool-block:hover :global(.tool-copy.kit-copy-btn),
  .tool-header-row:focus-within :global(.tool-copy.kit-copy-btn),
  .output-header-row:focus-within :global(.tool-copy.kit-copy-btn) { opacity: 1; }
  .history-header {
    display: flex; align-items: center; gap: 6px; padding: 5px 10px; width: 100%; text-align: left;
    font-size: 12px; color: var(--text-secondary); min-width: 0; border-top: 1px solid var(--border-muted);
    transition: background 0.1s; user-select: text;
  }
  .history-header:hover { background: var(--bg-surface-hover); color: var(--text-primary); }
  .output-label { font-family: var(--font-mono); font-weight: 500; font-size: 11px; color: var(--text-secondary); white-space: nowrap; flex-shrink: 0; }
  .output-content { max-height: 300px; overflow-y: auto; }
  .result-history, .result-event + .result-event { border-top: 1px solid var(--border-muted); }
  .result-event-meta { display: flex; flex-wrap: wrap; gap: 6px; padding: 6px 14px 0; }
  .history-content { border-top: 0; margin-top: 0; }
  .diff-view {
    font-family: var(--font-mono); font-size: 12px; line-height: 1.5; overflow-x: auto;
    border-top: 1px solid var(--border-muted); padding: 4px 0; max-height: 400px; overflow-y: auto;
  }
  .diff-line { display: inline-block; min-width: 100%; padding: 0 14px; white-space: pre; }
  .diff-hunk { color: var(--accent-blue, #58a6ff); background: color-mix(in srgb, var(--accent-blue, #58a6ff) 8%, transparent); padding: 2px 14px; margin: 2px 0; }
  .diff-add { color: var(--accent-green, #3fb950); background: color-mix(in srgb, var(--accent-green, #3fb950) 10%, transparent); }
  .diff-del { color: var(--accent-red, #f85149); background: color-mix(in srgb, var(--accent-red, #f85149) 10%, transparent); }
  .diff-ctx { color: var(--text-muted); }
</style>
