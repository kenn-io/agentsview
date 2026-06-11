<script lang="ts">
  import { onMount } from "svelte";
  import { insights } from "../../stores/insights.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { highlightCodeFences } from "../../utils/highlight-fences.js";
  import type { InsightType, AgentName } from "../../api/types.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import {
    LightbulbIcon,
    MousePointer2Icon,
    PencilIcon,
    PlusIcon,
    TrashIcon,
    TriangleAlertIcon,
    XIcon,
  } from "../../icons.js";

  type UIMode =
    | "daily_activity"
    | "range_activity"
    | "agent_analysis";

  let promptExpanded = $state(false);
  const readOnly = $derived(
    sync.serverVersion?.read_only === true,
  );
  const generationUnavailable = $derived(
    sync.serverVersion === null || readOnly,
  );

  const uiMode: UIMode = $derived.by(() => {
    if (insights.type === "agent_analysis") {
      return "agent_analysis";
    }
    if (insights.dateFrom !== insights.dateTo) {
      return "range_activity";
    }
    return "daily_activity";
  });

  function isRangeMode(mode: UIMode): boolean {
    return mode === "range_activity";
  }

  function handleModeChange(e: Event) {
    const select = e.target as HTMLSelectElement;
    const mode = select.value as UIMode;
    if (mode === "range_activity") {
      insights.setType("daily_activity");
      if (insights.dateFrom === insights.dateTo) {
        const d = new Date(
          insights.dateFrom + "T00:00:00",
        );
        d.setDate(d.getDate() + 6);
        insights.setDateTo(localDateStr(d));
      }
    } else {
      insights.setType(
        mode === "agent_analysis"
          ? "agent_analysis"
          : "daily_activity",
      );
      insights.setDateTo(insights.dateFrom);
    }
  }

  function handleDateChange(e: Event) {
    const input = e.target as HTMLInputElement;
    insights.setDateFrom(input.value);
    insights.setDateTo(input.value);
  }

  function handleDateFromChange(e: Event) {
    const input = e.target as HTMLInputElement;
    insights.setDateFrom(input.value);
    if (input.value > insights.dateTo) {
      insights.setDateTo(input.value);
    }
  }

  function handleDateToChange(e: Event) {
    const input = e.target as HTMLInputElement;
    insights.setDateTo(input.value);
    if (input.value < insights.dateFrom) {
      insights.setDateFrom(input.value);
    }
  }

  function setPreset(days: number) {
    const today = new Date();
    const from = new Date(today);
    from.setDate(from.getDate() - days);
    insights.setDateFrom(localDateStr(from));
    insights.setDateTo(localDateStr(today));
  }

  function localDateStr(d: Date): string {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
  }

  function handleProjectChange(value: string) {
    insights.setProject(value);
  }

  function handleAgentChange(e: Event) {
    const select = e.target as HTMLSelectElement;
    insights.setAgent(select.value as AgentName);
  }

  function handleGenerate() {
    if (generationUnavailable) return;
    insights.generate();
  }

  function formatTime(iso: string): string {
    const d = new Date(iso);
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function formatDate(date: string): string {
    const d = new Date(date + "T00:00:00");
    return d.toLocaleDateString(undefined, {
      weekday: "short",
      month: "short",
      day: "numeric",
    });
  }

  function formatDateShort(date: string): string {
    const d = new Date(date + "T00:00:00");
    return d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  }

  function formatDateRange(
    from: string,
    to: string,
  ): string {
    if (from === to) return formatDateShort(from);
    return `${formatDateShort(from)} – ${formatDateShort(to)}`;
  }

  function typeLabel(
    type: InsightType,
    from: string,
    to: string,
  ): string {
    if (type === "agent_analysis") return "Agent Analysis";
    return from === to
      ? "Daily Activity"
      : "Date Range Activity";
  }

  function typeShort(
    type: InsightType,
    from: string,
    to: string,
  ): string {
    if (type === "agent_analysis") return "Analysis";
    return from === to ? "Daily" : "Range";
  }

  onMount(() => {
    sessions.loadProjects();
    insights.load();
  });
</script>

<div class="insights-page">
  <div class="sidebar-panel">
    <div class="controls">
      <select
        class="ctrl mode-ctrl"
        value={uiMode}
        onchange={handleModeChange}
      >
        <option value="daily_activity">Daily Activity</option>
        <option value="range_activity">Date Range Activity</option>
        <option value="agent_analysis">Agent Analysis</option>
      </select>

      {#if isRangeMode(uiMode)}
        <div class="date-range-group">
          <div class="controls-row">
            <label class="date-label">
              <span class="date-label-text">From</span>
              <input
                type="date"
                class="ctrl date-ctrl"
                value={insights.dateFrom}
                onchange={handleDateFromChange}
              />
            </label>
            <label class="date-label">
              <span class="date-label-text">To</span>
              <input
                type="date"
                class="ctrl date-ctrl"
                value={insights.dateTo}
                onchange={handleDateToChange}
              />
            </label>
          </div>
          <div class="presets-row">
            <button class="preset-btn" onclick={() => setPreset(6)}>Last 7 days</button>
            <button class="preset-btn" onclick={() => setPreset(29)}>Last 30 days</button>
          </div>
        </div>
      {:else}
        <input
          type="date"
          class="ctrl date-ctrl"
          value={insights.dateFrom}
          onchange={handleDateChange}
        />
      {/if}

      <div class="controls-row">
        <ProjectTypeahead
          projects={sessions.projects}
          value={insights.project}
          onselect={handleProjectChange}
        />
        <select
          class="ctrl agent-ctrl"
          value={insights.agent}
          onchange={handleAgentChange}
        >
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
          <option value="copilot">Copilot</option>
          <option value="gemini">Gemini</option>
          <option value="kiro">Kiro</option>
        </select>
      </div>

      {#if promptExpanded}
        <textarea
          class="prompt-area"
          placeholder="Steer the insight with additional context..."
          bind:value={insights.promptText}
          rows="3"
        ></textarea>
      {/if}

      <div class="action-row">
        <button
          class="prompt-toggle"
          onclick={() => promptExpanded = !promptExpanded}
          title={promptExpanded ? "Hide prompt" : "Add custom prompt"}
        >
          <PencilIcon size="12" strokeWidth="2" aria-hidden="true" />
          {promptExpanded ? "Hide" : "Prompt"}
        </button>
        <button
          class="generate-btn"
          onclick={handleGenerate}
          disabled={insights.loading || generationUnavailable}
          title={readOnly
            ? "Unavailable in read-only remote mode"
            : sync.serverVersion === null
              ? "Waiting for server version"
              : "Generate insight"}
        >
          <PlusIcon class="generate-icon" size="12" strokeWidth="2.2" aria-hidden="true" />
          Generate
        </button>
      </div>
      {#if readOnly}
        <div class="readonly-note">
          Read-only remote mode cannot save generated insights.
        </div>
      {/if}
    </div>

    <div class="list-area">
      {#if insights.tasks.length > 0}
        <div class="list-section-header">
          <span class="section-title">
            {#if insights.generatingCount > 0}
              <span class="live-dot"></span>
            {/if}
            Tasks
            <span class="active-count">{insights.tasks.length}</span>
          </span>
          {#if insights.generatingCount > 1}
            <button
              class="cancel-all"
              onclick={() => insights.cancelAll()}
            >
              Stop all
            </button>
          {/if}
        </div>
        {#each insights.tasks as task (task.clientId)}
          <div
            class="task-item"
            class:task-error={task.status === "error"}
            class:selected={insights.selectedTaskId === task.clientId}
            role="button"
            tabindex="0"
            onclick={() => insights.selectTask(task.clientId)}
            onkeydown={(e) => { if (e.target === e.currentTarget && (e.key === "Enter" || e.key === " ")) insights.selectTask(task.clientId); }}
          >
            <div class="task-indicator">
              {#if task.status === "generating"}
                <span class="spinner"></span>
              {:else}
                <span class="error-pip"></span>
              {/if}
            </div>
            <div class="task-body">
              <div class="task-main">
                <span class="task-label">
                  {typeShort(task.type, task.dateFrom, task.dateTo)}
                  <span class="task-date">
                    {formatDateRange(task.dateFrom, task.dateTo)}
                  </span>
                </span>
                <span class="task-scope">
                  {task.project || "global"}
                </span>
              </div>
              {#if task.status === "error"}
                <span class="task-error-msg">{task.error}</span>
              {:else}
                <span class="task-phase">{task.phase}</span>
              {/if}
            </div>
            <span class="task-agent">{task.agent}</span>
            <button
              class="task-dismiss"
              onclick={(e) => {
                e.stopPropagation();
                if (task.status === "error") {
                  insights.dismissTask(task.clientId);
                } else {
                  insights.cancelTask(task.clientId);
                }
              }}
              title={task.status === "error" ? "Dismiss" : "Cancel"}
            >
              <XIcon size="8" strokeWidth="2.4" aria-hidden="true" />
            </button>
            {#if task.status === "generating"}
              <div class="shimmer-bar"></div>
            {/if}
          </div>
        {/each}
      {/if}

      {#if insights.loading}
        <div class="list-status">Loading...</div>
      {:else if insights.items.length === 0 && insights.tasks.length === 0}
        <div class="empty-state">
          <div class="empty-glyph">
            <LightbulbIcon size="28" strokeWidth="1.5" aria-hidden="true" />
          </div>
          <span class="empty-text">
            Generate an insight to analyze your sessions
          </span>
        </div>
      {:else}
        {#if insights.tasks.length > 0}
          <div class="list-section-header completed-header">
            <span class="section-title">Completed</span>
          </div>
        {/if}
        {#each insights.items as s (s.id)}
          <button
            class="insight-row"
            class:selected={insights.selectedId === s.id}
            onclick={() => insights.select(s.id)}
          >
            <span
              class="type-pip"
              class:pip-blue={s.type === "daily_activity"}
              class:pip-purple={s.type === "agent_analysis"}
            ></span>
            <span class="row-body">
              <span class="row-title">
                {typeShort(s.type, s.date_from, s.date_to)}
                <span class="row-scope">
                  {s.project || "global"}
                </span>
              </span>
              <span class="row-meta">
                {formatDateRange(s.date_from, s.date_to)}
                <span class="row-time">
                  {formatTime(s.created_at)}
                </span>
              </span>
            </span>
            <span class="row-agent">{s.agent}</span>
          </button>
        {/each}
      {/if}
    </div>
  </div>

  <main class="content-panel">
    {#if insights.selectedTask}
      {@const task = insights.selectedTask}
      <div class="reading-area">
        <header class="insight-header">
          <div class="header-top">
            <span
              class="header-badge"
              class:badge-red={task.status === "error"}
              class:badge-blue={task.status !== "error"}
            >
              {task.status === "error" ? "Error" : "Generating"}
            </span>
            <span class="header-date">
              {typeShort(task.type, task.dateFrom, task.dateTo)}
              {formatDateRange(task.dateFrom, task.dateTo)}
            </span>
            <button
              class="delete-btn"
              onclick={() => task.status === "error"
                ? insights.dismissTask(task.clientId)
                : insights.cancelTask(task.clientId)}
              title={task.status === "error" ? "Dismiss" : "Cancel"}
            >
              <XIcon size="14" strokeWidth="2.2" aria-hidden="true" />
            </button>
          </div>
          <div class="header-details">
            {#if task.project}
              <span class="detail-chip">{task.project}</span>
            {:else}
              <span class="detail-chip muted">global</span>
            {/if}
            <span class="detail-text">{task.agent}</span>
          </div>
        </header>
        {#if task.status === "error" && task.error}
          <div class="task-error-banner">
            <TriangleAlertIcon size="14" strokeWidth="1.8" aria-hidden="true" />
            <span>{task.error}</span>
          </div>
        {/if}
        {#if task.logs.length > 0}
          <div class="task-detail-logs" role="log">
            <div class="task-detail-logs-header">
              Execution Log
              <span class="log-count">{task.logs.length} lines</span>
            </div>
            <div class="task-detail-logs-body">
              {#each task.logs as entry}
                <div
                  class="task-log-line"
                  class:log-stderr={entry.stream === "stderr"}
                >
                  <span class="task-log-stream">{entry.stream}</span>
                  <span class="task-log-text">{entry.line}</span>
                </div>
              {/each}
            </div>
          </div>
        {:else if task.status === "generating"}
          <div class="content-generating" style="margin-top: 48px">
            <div class="gen-orbit">
              <span class="orbit-ring"></span>
              <span class="orbit-dot"></span>
            </div>
            <span class="gen-label">Waiting for {task.agent}...</span>
          </div>
        {/if}
      </div>
    {:else if insights.selectedItem}
      <div class="reading-area">
        <header class="insight-header">
          <div class="header-top">
            <span
              class="header-badge"
              class:badge-blue={insights.selectedItem.type === "daily_activity"}
              class:badge-purple={insights.selectedItem.type === "agent_analysis"}
            >
              {typeLabel(insights.selectedItem.type, insights.selectedItem.date_from, insights.selectedItem.date_to)}
            </span>
            <span class="header-date">
              {#if insights.selectedItem.date_from === insights.selectedItem.date_to}
                {formatDate(insights.selectedItem.date_from)}
              {:else}
                {formatDateShort(insights.selectedItem.date_from)} – {formatDateShort(insights.selectedItem.date_to)}
              {/if}
            </span>
            <button
              class="delete-btn"
              onclick={() => {
                if (insights.selectedItem) {
                  insights.deleteItem(insights.selectedItem.id);
                }
              }}
              title="Delete this insight"
            >
              <TrashIcon size="14" strokeWidth="1.8" aria-hidden="true" />
            </button>
          </div>
          <div class="header-details">
            {#if insights.selectedItem.project}
              <span class="detail-chip">{insights.selectedItem.project}</span>
            {:else}
              <span class="detail-chip muted">global</span>
            {/if}
            <span class="detail-text">
              {insights.selectedItem.agent}
              {#if insights.selectedItem.model}
                <span class="model-name">{insights.selectedItem.model}</span>
              {/if}
            </span>
            <span class="detail-time">
              {formatTime(insights.selectedItem.created_at)}
            </span>
          </div>
        </header>
        <article
          class="markdown-body"
          use:highlightCodeFences={{ content: insights.selectedItem.content }}
        >
          {@html renderMarkdown(insights.selectedItem.content)}
        </article>
      </div>
    {:else}
      <div class="content-empty">
        {#if insights.items.length > 0}
          <div class="empty-prompt">
            <MousePointer2Icon size="20" strokeWidth="1.5" aria-hidden="true" />
            <span>Select an insight to view</span>
          </div>
        {:else if insights.tasks.length > 0}
          <div class="content-generating">
            <div class="gen-orbit">
              <span class="orbit-ring"></span>
              <span class="orbit-dot"></span>
            </div>
            <span class="gen-label">Generating insight...</span>
          </div>
        {:else}
          <div class="empty-prompt">
            <LightbulbIcon size="20" strokeWidth="1.5" aria-hidden="true" />
            <span>Generate an insight to get started</span>
          </div>
        {/if}
      </div>
    {/if}
  </main>
</div>

<style>
  /* ── Layout ── */
  .insights-page {
    display: grid;
    grid-template-columns: 280px 1fr;
    height: calc(100vh - 40px - 24px);
    height: calc(100dvh - 40px - 24px);
    overflow: hidden;
  }

  /* ── Sidebar ── */
  .sidebar-panel {
    display: flex;
    flex-direction: column;
    border-right: 1px solid var(--border-default);
    background: var(--bg-surface);
    overflow: hidden;
  }

  /* ── Controls ── */
  .controls {
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 6px;
    border-bottom: 1px solid var(--border-default);
    flex-shrink: 0;
  }

  .controls-row {
    display: flex;
    gap: 6px;
  }

  .ctrl {
    flex: 1;
    height: 26px;
    padding: 0 6px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-secondary);
    min-width: 0;
    transition: border-color 0.15s;
  }

  .ctrl:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .mode-ctrl {
    width: 100%;
    flex: none;
  }

  .date-ctrl {
    flex: 1;
  }

  .date-range-group {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .date-label {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .date-label-text {
    font-size: 10px;
    color: var(--text-muted);
    padding-left: 2px;
  }

  .presets-row {
    display: flex;
    gap: 4px;
  }

  .preset-btn {
    height: 22px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 10px;
    color: var(--text-muted);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    transition: background 0.1s, color 0.1s;
    white-space: nowrap;
  }

  .preset-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .prompt-area {
    width: 100%;
    padding: 6px 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-primary);
    font-family: var(--font-sans);
    resize: vertical;
    min-height: 48px;
    line-height: 1.4;
    transition: border-color 0.15s;
  }

  .prompt-area:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .prompt-area::placeholder {
    color: var(--text-muted);
  }

  .action-row {
    display: flex;
    gap: 6px;
    align-items: center;
  }

  .prompt-toggle {
    display: flex;
    align-items: center;
    gap: 4px;
    height: 26px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-muted);
    transition: background 0.1s, color 0.1s;
  }

  .prompt-toggle:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .generate-btn {
    flex: 1;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 5px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 600;
    background: var(--accent-blue);
    color: white;
    letter-spacing: 0.01em;
    transition: opacity 0.12s, transform 0.1s,
      box-shadow 0.12s;
    box-shadow: 0 1px 2px rgba(37, 99, 235, 0.2);
  }

  .generate-btn:hover:not(:disabled) {
    opacity: 0.92;
    box-shadow: 0 2px 6px rgba(37, 99, 235, 0.3);
  }

  .generate-btn:active:not(:disabled) {
    transform: scale(0.98);
    box-shadow: none;
  }

  .generate-btn:disabled {
    opacity: 0.45;
    box-shadow: none;
  }

  :global(.generate-icon) {
    opacity: 0.9;
  }

  .readonly-note {
    padding: 4px 2px 0;
    font-size: 10px;
    line-height: 1.35;
    color: var(--text-muted);
  }

  /* ── List Area ── */
  .list-area {
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
  }

  .list-section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 14px 6px;
    position: sticky;
    top: 0;
    z-index: 1;
    background: var(--bg-surface);
  }

  .section-title {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .live-dot {
    width: 5px;
    height: 5px;
    border-radius: 50%;
    background: var(--accent-green);
    animation: blink 1.6s ease-in-out infinite;
  }

  .active-count {
    font-variant-numeric: tabular-nums;
    color: var(--text-muted);
    font-weight: 500;
  }

  .cancel-all {
    font-size: 10px;
    color: var(--text-muted);
    transition: color 0.1s;
  }

  .cancel-all:hover {
    color: var(--accent-red);
  }

  .completed-header {
    border-top: 1px solid var(--border-muted);
  }

  /* ── Task Items (generating) ── */
  .task-item {
    position: relative;
    display: flex;
    align-items: flex-start;
    gap: 10px;
    min-height: 42px;
    padding: 8px 14px 10px;
    overflow: hidden;
    width: 100%;
    text-align: left;
    border-left: 2px solid transparent;
    transition: background 0.1s;
    cursor: pointer;
  }

  .task-item:hover {
    background: var(--bg-surface-hover);
  }

  .task-item.selected {
    background: var(--bg-surface-hover);
    border-left-color: var(--accent-blue);
  }

  .task-item.selected.task-error {
    border-left-color: var(--accent-red);
  }

  .task-error {
    background: color-mix(
      in srgb,
      var(--accent-red) 6%,
      transparent
    );
  }

  .task-indicator {
    flex-shrink: 0;
    width: 14px;
    margin-top: 2px;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .spinner {
    width: 10px;
    height: 10px;
    border: 1.5px solid var(--accent-blue);
    border-top-color: transparent;
    border-radius: 50%;
    animation: spin 0.7s linear infinite;
  }

  .error-pip {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent-red);
  }

  .task-body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    line-height: 1.35;
  }

  .task-main {
    width: 100%;
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    min-width: 0;
  }

  .task-label {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .task-date {
    font-weight: 400;
    color: var(--text-muted);
    margin-left: 4px;
  }

  .task-scope {
    font-size: 10px;
    color: var(--text-muted);
    white-space: nowrap;
    text-overflow: ellipsis;
    overflow: hidden;
    max-width: 45%;
  }

  .task-phase {
    width: 100%;
    font-size: 10px;
    color: var(--accent-blue);
    font-family: var(--font-mono);
    letter-spacing: -0.02em;
    word-break: break-word;
  }

  .task-error-msg {
    width: 100%;
    font-size: 10px;
    color: var(--accent-red);
    word-break: break-word;
  }

  /* ── Task Detail (main pane) ── */
  .task-error-banner {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    padding: 12px 16px;
    border-radius: var(--radius-md);
    background: color-mix(
      in srgb,
      var(--accent-red) 8%,
      var(--bg-inset)
    );
    border: 1px solid color-mix(
      in srgb,
      var(--accent-red) 25%,
      var(--border-muted)
    );
    color: var(--accent-red);
    font-size: 13px;
    line-height: 1.5;
    margin-bottom: 20px;
  }

  .task-error-banner :global(svg) {
    flex-shrink: 0;
    margin-top: 2px;
    opacity: 0.8;
  }

  .task-detail-logs {
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .task-detail-logs-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 8px 14px;
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-muted);
    font-size: 11px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .log-count {
    font-weight: 400;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  .task-detail-logs-body {
    max-height: 50vh;
    overflow-y: auto;
    padding: 8px 14px;
    font-family: var(--font-mono);
    font-size: 11px;
    line-height: 1.5;
  }

  .task-log-line {
    display: grid;
    grid-template-columns: 48px 1fr;
    gap: 8px;
    color: var(--text-secondary);
    white-space: pre-wrap;
    word-break: break-word;
  }

  .task-log-stream {
    text-transform: uppercase;
    color: var(--text-muted);
    font-size: 10px;
  }

  .task-log-text {
    min-width: 0;
  }

  .log-stderr .task-log-stream,
  .log-stderr .task-log-text {
    color: var(--accent-red);
  }

  .task-agent {
    flex-shrink: 0;
    font-size: 10px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    letter-spacing: -0.02em;
    white-space: nowrap;
    margin-top: 2px;
  }

  .task-dismiss {
    flex-shrink: 0;
    width: 18px;
    height: 18px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    opacity: 0;
    margin-top: 1px;
    transition: opacity 0.15s, background 0.1s, color 0.1s;
  }

  .task-item:hover .task-dismiss,
  .task-dismiss:focus-visible {
    opacity: 1;
  }

  @media (hover: none) {
    .task-dismiss {
      opacity: 0.7;
    }
  }

  .task-dismiss:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .shimmer-bar {
    position: absolute;
    bottom: 0;
    left: 0;
    right: 0;
    height: 1px;
    background: linear-gradient(
      90deg,
      transparent 0%,
      var(--accent-blue) 50%,
      transparent 100%
    );
    background-size: 200% 100%;
    animation: shimmer 1.8s ease-in-out infinite;
  }

  /* ── Insight Rows (completed) ── */
  .insight-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 42px;
    padding: 0 14px;
    text-align: left;
    border-left: 2px solid transparent;
    transition: background 0.1s;
  }

  .insight-row:hover {
    background: var(--bg-surface-hover);
  }

  .insight-row.selected {
    background: var(--bg-surface-hover);
    border-left-color: var(--accent-blue);
  }

  .type-pip {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .pip-blue {
    background: var(--accent-blue);
  }

  .pip-purple {
    background: var(--accent-purple);
  }

  .row-body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .row-title {
    font-size: 12px;
    font-weight: 450;
    color: var(--text-primary);
    line-height: 1.3;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    letter-spacing: -0.005em;
  }

  .row-scope {
    color: var(--text-muted);
    margin-left: 4px;
    font-weight: 400;
  }

  .row-meta {
    font-size: 10px;
    color: var(--text-muted);
    line-height: 1.3;
  }

  .row-time {
    margin-left: 4px;
    opacity: 0.7;
  }

  .row-agent {
    flex-shrink: 0;
    font-size: 10px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    letter-spacing: -0.02em;
    white-space: nowrap;
    max-width: 60px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .list-status {
    padding: 16px 12px;
    font-size: 11px;
    color: var(--text-muted);
    text-align: center;
  }

  /* ── Empty State ── */
  .empty-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
    padding: 32px 16px;
    text-align: center;
  }

  .empty-glyph {
    color: var(--text-muted);
    opacity: 0.4;
  }

  .empty-text {
    font-size: 11px;
    color: var(--text-muted);
    line-height: 1.5;
    max-width: 180px;
  }

  /* ── Content Panel ── */
  .content-panel {
    overflow: hidden;
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
  }

  .reading-area {
    flex: 1;
    overflow-y: auto;
    padding: 28px 36px 48px;
  }

  .insight-header {
    margin-bottom: 24px;
    padding-bottom: 16px;
    border-bottom: 1px solid var(--border-muted);
  }

  .header-top {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 8px;
  }

  .header-badge {
    font-size: 9px;
    font-weight: 700;
    padding: 3px 8px;
    border-radius: 10px;
    color: white;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .badge-blue {
    background: var(--accent-blue);
  }

  .badge-purple {
    background: var(--accent-purple);
  }

  .badge-red {
    background: var(--accent-red);
  }

  .header-date {
    font-size: 15px;
    font-weight: 600;
    color: var(--text-primary);
    letter-spacing: -0.01em;
  }

  .delete-btn {
    margin-left: auto;
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    transition: background 0.12s, color 0.12s;
  }

  .delete-btn:hover {
    background: color-mix(
      in srgb,
      var(--accent-red) 10%,
      transparent
    );
    color: var(--accent-red);
  }

  .header-details {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .detail-chip {
    padding: 1px 6px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: 11px;
  }

  .detail-chip.muted {
    color: var(--text-muted);
    font-style: italic;
  }

  .detail-text {
    color: var(--text-muted);
  }

  .model-name {
    font-family: var(--font-mono);
    font-size: 10px;
    opacity: 0.7;
    margin-left: 2px;
  }

  .detail-time {
    margin-left: auto;
    font-variant-numeric: tabular-nums;
  }

  .content-empty {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .empty-prompt {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
    color: var(--text-muted);
    opacity: 0.5;
    font-size: 12px;
  }

  .content-generating {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 16px;
    color: var(--text-muted);
  }

  .gen-orbit {
    position: relative;
    width: 36px;
    height: 36px;
  }

  .orbit-ring {
    position: absolute;
    inset: 0;
    border: 1.5px solid var(--border-muted);
    border-radius: 50%;
  }

  .orbit-dot {
    position: absolute;
    width: 6px;
    height: 6px;
    background: var(--accent-blue);
    border-radius: 50%;
    top: -3px;
    left: 50%;
    margin-left: -3px;
    animation: orbit 1.5s linear infinite;
    transform-origin: 3px 21px;
  }

  .gen-label {
    font-size: 12px;
    color: var(--text-muted);
  }

  /* ── Markdown Content ── */
  .markdown-body {
    font-size: 14px;
    line-height: 1.7;
    color: var(--text-primary);
    max-width: 720px;
  }

  .markdown-body :global(h1) {
    font-size: 20px;
    font-weight: 700;
    margin: 0 0 14px;
    padding-bottom: 8px;
    border-bottom: 1px solid var(--border-muted);
    letter-spacing: -0.02em;
  }

  .markdown-body :global(h2) {
    font-size: 16px;
    font-weight: 600;
    margin: 28px 0 10px;
    letter-spacing: -0.015em;
  }

  .markdown-body :global(h3) {
    font-size: 14px;
    font-weight: 600;
    margin: 20px 0 6px;
    letter-spacing: -0.01em;
  }

  .markdown-body :global(p) {
    margin: 0 0 10px;
  }

  .markdown-body :global(ul),
  .markdown-body :global(ol) {
    margin: 0 0 10px;
    padding-left: 20px;
  }

  .markdown-body :global(li) {
    margin: 3px 0;
  }

  .markdown-body :global(li + li) {
    margin-top: 4px;
  }

  .markdown-body :global(code) {
    font-family: var(--font-mono);
    font-size: 12px;
    padding: 2px 5px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
  }

  .markdown-body :global(pre) {
    background: var(--bg-inset);
    padding: 10px 14px;
    border-radius: var(--radius-md);
    overflow-x: auto;
    margin: 0 0 10px;
    border: 1px solid var(--border-muted);
  }

  .markdown-body :global(pre code) {
    padding: 0;
    background: transparent;
    border: none;
  }

  .markdown-body :global(blockquote) {
    margin: 0 0 10px;
    padding: 6px 14px;
    border-left: 3px solid var(--accent-blue);
    color: var(--text-secondary);
    background: color-mix(
      in srgb,
      var(--accent-blue) 4%,
      transparent
    );
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  }

  .markdown-body :global(strong) {
    font-weight: 600;
    color: var(--text-primary);
  }

  .markdown-body :global(a) {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .markdown-body :global(a:hover) {
    text-decoration: underline;
  }

  .markdown-body :global(hr) {
    border: none;
    border-top: 1px solid var(--border-muted);
    margin: 20px 0;
  }

  .markdown-body :global(table) {
    width: 100%;
    border-collapse: collapse;
    margin: 0 0 10px;
    font-size: 12px;
  }

  .markdown-body :global(th),
  .markdown-body :global(td) {
    padding: 6px 10px;
    border: 1px solid var(--border-muted);
    text-align: left;
  }

  .markdown-body :global(th) {
    background: var(--bg-inset);
    font-weight: 600;
  }

  /* ── Animations ── */
  @keyframes spin {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }

  @keyframes shimmer {
    0% { background-position: 200% 0; }
    100% { background-position: -200% 0; }
  }

  @keyframes blink {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.3; }
  }

  @keyframes orbit {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }
</style>
