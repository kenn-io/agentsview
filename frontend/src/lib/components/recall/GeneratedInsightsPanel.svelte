<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import {
    Button,
    Card,
    CopyButton,
    IconButton,
    Typeahead,
  } from "@kenn-io/kit-ui";
  import { downloadInsightExport } from "../../api/client.js";
  import type {
    AgentName,
    AutomatedScope,
    CannedInsightKind,
  } from "../../api/types.js";
  import { m } from "../../i18n/index.js";
  import { router, getBasePath } from "../../stores/router.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { agentLabel } from "../../utils/agents.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    selectionFromRange,
    type RangeSelection,
  } from "../shared/rangeSelection.js";

  let copiedInsightLinkId: number | null = $state(null);
  let copiedInsightLinkTimer: ReturnType<typeof setTimeout> | undefined;

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);
  const rangeSelection = $derived(
    selectionFromRange(
      insights.dateFrom,
      insights.dateTo,
      earliestSession,
    ),
  );
  const insightGenerationAvailable = $derived(
    sync.serverVersion?.insight_generation_available === true ||
      sync.serverVersion?.read_only !== true,
  );
  const generationUnavailable = $derived(
    sync.serverVersion === null || !insightGenerationAvailable,
  );

  const generationAgentNames = [
    "claude",
    "codex",
    "copilot",
    "gemini",
    "kiro",
  ] satisfies AgentName[];
  const generationAgentOptions = generationAgentNames.map((name) => ({
    name,
    label: agentLabel(name),
    displayLabel: agentLabel(name),
  }));
  const sessionAgentOptions = $derived.by(() => [
    {
      name: "",
      label: m.insights_page_all_agents(),
      displayLabel: m.insights_page_all_agents(),
    },
    ...[...sessions.agents]
      .sort((a, b) => b.session_count - a.session_count)
      .map((agent) => ({
        name: agent.name,
        label: `${agentLabel(agent.name)} (${agent.session_count})`,
        displayLabel: agentLabel(agent.name),
        count: agent.session_count,
      })),
  ]);
  const templateOptions = $derived([
    {
      name: "prompt_maturity_review",
      label: m.insights_page_template_prompt_maturity(),
    },
    {
      name: "context_setup_review",
      label: m.insights_page_template_context_setup(),
    },
    {
      name: "workflow_hygiene_review",
      label: m.insights_page_template_workflow_hygiene(),
    },
    {
      name: "tool_reliability_review",
      label: m.insights_page_template_tool_reliability(),
    },
    {
      name: "model_cost_review",
      label: m.insights_page_template_model_cost(),
    },
    {
      name: "instruction_opportunity_review",
      label: m.insights_page_template_instruction_opportunities(),
    },
  ]);
  const scopeOptions = $derived([
    { name: "human", label: m.insights_page_scope_no_automated() },
    { name: "all", label: m.insights_page_scope_both() },
    {
      name: "automated",
      label: m.insights_page_scope_only_automated(),
    },
  ]);

  function applyRange(selection: RangeSelection) {
    const range = resolveRange(selection, earliestSession);
    insights.setDateFrom(range.from);
    insights.setDateTo(range.to);
  }

  function handlePromptChange(event: Event) {
    insights.promptText = (event.target as HTMLTextAreaElement).value;
  }

  function handleGenerate() {
    if (generationUnavailable) return;
    insights.setType("llm_canned");
    insights.generate();
  }

  function generatedParams(
    extra: Record<string, string> = {},
  ): Record<string, string> {
    return { ...router.params, tab: "generated", ...extra };
  }

  function selectGeneratedInsight(id: number) {
    insights.select(id);
    router.replaceParams(generatedParams({ insight: String(id) }));
  }

  function selectGeneratedTask(clientId: string) {
    insights.selectTask(clientId);
    const params = generatedParams();
    delete params.insight;
    router.replaceParams(params);
  }

  function insightLinkPath(id: number): string {
    const params = new URLSearchParams(generatedParams({
      insight: String(id),
    }));
    return `${getBasePath()}/recall?${params.toString()}`;
  }

  async function handleCopyInsightLink(id: number) {
    const url = new URL(insightLinkPath(id), window.location.origin).toString();
    if (!await copyToClipboard(url)) return;
    copiedInsightLinkId = id;
    clearTimeout(copiedInsightLinkTimer);
    copiedInsightLinkTimer = setTimeout(() => {
      copiedInsightLinkId = null;
    }, 1500);
  }

  async function handleInsightExport() {
    if (!insights.selectedItem) return;
    try {
      await downloadInsightExport(insights.selectedItem.id);
    } catch (error) {
      console.error("Insight export failed:", error);
    }
  }

  function openInsightPublish(secret: boolean) {
    if (!insights.selectedItem) return;
    ui.publishSecret = secret;
    ui.setPublishTarget({
      kind: "insight",
      id: insights.selectedItem.id,
    });
    ui.activeModal = "publish";
  }

  function deleteSelectedInsight() {
    if (!insights.selectedItem) return;
    void insights.deleteItem(insights.selectedItem.id);
    const params = generatedParams();
    delete params.insight;
    router.replaceParams(params);
  }

  function selectedInsightFromRoute(): number | null {
    const raw = router.params.insight;
    if (!raw) return null;
    const id = Number.parseInt(raw, 10);
    return Number.isSafeInteger(id) && id > 0 ? id : null;
  }

  function formatDate(date: string): string {
    return new Date(`${date}T00:00:00`).toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  }

  function formatDateRange(from: string, to: string): string {
    return from === to
      ? formatDate(from)
      : m.insights_page_date_range({
          from: formatDate(from),
          to: formatDate(to),
        });
  }

  function formatTime(value: string): string {
    return new Date(value).toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function cannedKindLabel(
    kind: string | undefined,
  ): string {
    switch (kind) {
      case "prompt_maturity_review":
        return m.insights_page_template_prompt_maturity();
      case "context_setup_review":
        return m.insights_page_template_context_setup();
      case "workflow_hygiene_review":
        return m.insights_page_template_workflow_hygiene();
      case "tool_reliability_review":
        return m.insights_page_template_tool_reliability();
      case "model_cost_review":
        return m.insights_page_template_model_cost();
      case "instruction_opportunity_review":
        return m.insights_page_template_instruction_opportunities();
      default:
        return m.insights_page_generated_recommendation();
    }
  }

  function insightTypeLabel(
    type: string,
    kind: string | undefined,
  ): string {
    if (type === "llm_canned") return cannedKindLabel(kind);
    if (type === "agent_analysis") return m.insights_page_agent_analysis();
    return m.insights_page_activity();
  }

  function cacheStatusLabel(status: string | undefined): string {
    if (status === "hit") return m.insights_page_cache_hit();
    if (status === "fresh") return m.insights_page_fresh();
    return "";
  }

  onMount(() => {
    void sessions.loadProjects();
    void sessions.loadAgents();
    void insights.load();
  });

  onDestroy(() => {
    insights.cancelInFlightReads();
    clearTimeout(copiedInsightLinkTimer);
  });

  $effect(() => {
    const id = selectedInsightFromRoute();
    if (id === null || insights.selectedId === id) return;
    if (insights.items.some((item) => item.id === id)) insights.select(id);
  });
</script>

<section class="generated-insights-panel" aria-labelledby="generated-title">
  <header class="generated-heading">
    <div>
      <h2 id="generated-title">{m.recall_page_tab_generated()}</h2>
      <p>{m.recall_generated_subtitle()}</p>
    </div>
  </header>

  <Card level="default" padding="none" class="scope-card">
    <div class="scope-grid">
      <div class="scope-field date-field">
        <span>{m.recall_generated_date_range()}</span>
        <RangePicker
          selection={rangeSelection}
          onSelect={applyRange}
          {earliestSession}
          block
        />
      </div>
      <div class="scope-field">
        <span>{m.recall_generated_project()}</span>
        <ProjectTypeahead
          projects={sessions.projects}
          value={insights.project}
          onselect={(value) => insights.setProject(value)}
        />
      </div>
      <div class="scope-field">
        <span>{m.recall_generated_session_agent()}</span>
        <Typeahead
          options={sessionAgentOptions}
          value={insights.sessionAgent}
          fallbackLabel={insights.sessionAgent
            ? agentLabel(insights.sessionAgent)
            : m.insights_page_all_agents()}
          placeholder={m.insights_page_filter_agents()}
          title={m.recall_generated_select_session_agent()}
          emptyLabel={m.insights_page_no_matching_agents()}
          onselect={(value) => insights.setSessionAgent(value)}
        />
      </div>
      <div class="scope-field">
        <span>{m.insights_page_session_scope()}</span>
        <Typeahead
          options={scopeOptions}
          value={insights.automatedScope}
          fallbackLabel={m.insights_page_scope_no_automated()}
          placeholder={m.insights_page_filter_scopes()}
          title={m.insights_page_filter_by_scope()}
          emptyLabel={m.insights_page_no_matching_scopes()}
          onselect={(value) =>
            insights.setAutomatedScope(value as AutomatedScope)}
        />
      </div>
      <div class="scope-field">
        <span>{m.insights_page_template_label()}</span>
        <Typeahead
          options={templateOptions}
          value={insights.cannedKind}
          fallbackLabel={cannedKindLabel(insights.cannedKind)}
          placeholder={m.insights_page_filter_templates()}
          title={m.insights_page_select_template()}
          emptyLabel={m.insights_page_no_matching_templates()}
          onselect={(value) =>
            insights.setCannedKind(value as CannedInsightKind)}
        />
      </div>
      <div class="scope-field">
        <span>{m.insights_page_generator_label()}</span>
        <Typeahead
          options={generationAgentOptions}
          value={insights.agent}
          fallbackLabel={agentLabel(insights.agent)}
          placeholder={m.insights_page_filter_generators()}
          title={m.insights_page_select_generator()}
          emptyLabel={m.insights_page_no_matching_generators()}
          onselect={(value) => insights.setAgent(value as AgentName)}
        />
      </div>
      <label class="scope-field focus-field">
        <span>{m.insights_page_optional_focus()}</span>
        <textarea
          value={insights.promptText}
          maxlength="1200"
          rows="2"
          placeholder={m.insights_page_focus_placeholder()}
          oninput={handlePromptChange}
        ></textarea>
      </label>
      <Button
        class="generate-action"
        label={m.insights_page_generate()}
        disabled={generationUnavailable}
        title={sync.serverVersion !== null && !insightGenerationAvailable
          ? m.insights_page_generate_disabled()
          : m.insights_page_generate_title()}
        onclick={handleGenerate}
      />
    </div>
  </Card>

  {#if insights.loading}
    <Card level="default" class="archive-state">
      {m.insights_page_loading_archive()}
    </Card>
  {:else if insights.items.length === 0 && insights.tasks.length === 0}
    <Card level="default" class="archive-state">
      <strong>{m.insights_page_no_generated_saved()}</strong>
      <span>{m.insights_page_no_generated_hint()}</span>
    </Card>
  {:else}
    <div class="generated-layout">
      <div class="generated-list" aria-label={m.recall_generated_archive_label()}>
        {#each insights.tasks as task (task.clientId)}
          <button
            class:error-task={task.status === "error"}
            class:selected={insights.selectedTaskId === task.clientId}
            onclick={() => selectGeneratedTask(task.clientId)}
          >
            <span>{task.status === "error"
              ? m.insights_page_error()
              : m.insights_page_running()}</span>
            <strong>{task.project || m.insights_page_global()}</strong>
            <em>{task.kind ? cannedKindLabel(task.kind) : task.phase}</em>
          </button>
        {/each}
        {#each insights.items as item (item.id)}
          <button
            class:selected={insights.selectedId === item.id}
            onclick={() => selectGeneratedInsight(item.id)}
          >
            <span>{insightTypeLabel(item.type, item.kind)}</span>
            <strong>{item.project || m.insights_page_global()}</strong>
            <em>{formatDateRange(item.date_from, item.date_to)} · {formatTime(item.created_at)}</em>
          </button>
        {/each}
      </div>

      <Card level="default" padding="none" class="generated-detail">
        <article>
          {#if insights.selectedTask}
            <div class="generated-detail-head">
              <span class="generated-badge">
                {insights.selectedTask.status === "error"
                  ? m.insights_page_generation_error()
                  : m.insights_page_generating()}
              </span>
              {#if insights.selectedTask.status === "error"}
                <div class="generated-actions">
                  <Button
                    size="sm"
                    label={m.insights_page_retry()}
                    onclick={() =>
                      insights.retryTask(insights.selectedTask!.clientId)}
                  />
                  <IconButton
                    size="sm"
                    tone="danger"
                    title={m.insights_page_dismiss_failed()}
                    ariaLabel={m.insights_page_dismiss_failed()}
                    onclick={() =>
                      insights.dismissTask(insights.selectedTask!.clientId)}
                  >×</IconButton>
                </div>
              {/if}
            </div>
            <p>{insights.selectedTask.error || insights.selectedTask.phase}</p>
          {:else if insights.selectedItem}
            <div class="generated-detail-head">
              <div class="generated-meta">
                <span class="generated-badge">
                  {insightTypeLabel(
                    insights.selectedItem.type,
                    insights.selectedItem.kind,
                  )}
                </span>
                {#if insights.selectedItem.type === "llm_canned"}
                  {#if cacheStatusLabel(insights.selectedItem.cache_status)}
                    <span class="generated-meta-chip">
                      {cacheStatusLabel(insights.selectedItem.cache_status)}
                    </span>
                  {/if}
                  {#if insights.selectedItem.template_version}
                    <span class="generated-meta-chip">
                      {m.insights_page_template_version({ version: insights.selectedItem.template_version })}
                    </span>
                  {/if}
                  {#if insights.selectedItem.aggregate_hash}
                    <span class="generated-meta-chip">
                      {m.insights_page_aggregate_hash({ hash: insights.selectedItem.aggregate_hash.slice(0, 12) })}
                    </span>
                  {/if}
                {/if}
              </div>
              <div class="generated-actions">
                <Button
                  size="sm"
                  label={m.recall_generated_export()}
                  onclick={handleInsightExport}
                />
                <Button
                  size="sm"
                  label={m.recall_generated_publish()}
                  onclick={() => openInsightPublish(false)}
                />
                <Button
                  size="sm"
                  surface="outline"
                  label={m.recall_generated_publish_secret()}
                  onclick={() => openInsightPublish(true)}
                />
                <CopyButton
                  copied={copiedInsightLinkId === insights.selectedItem.id}
                  ariaLabel={m.insights_page_copy_link()}
                  copiedAriaLabel={m.insights_page_copied_link()}
                  title={m.insights_page_copy_link_title()}
                  copiedTitle={m.insights_page_copied_link_short()}
                  onclick={() =>
                    handleCopyInsightLink(insights.selectedItem!.id)}
                />
                <IconButton
                  size="sm"
                  tone="danger"
                  title={m.insights_page_delete_insight()}
                  ariaLabel={m.insights_page_delete_insight()}
                  onclick={deleteSelectedInsight}
                >×</IconButton>
              </div>
            </div>
            <div class="markdown-body">
              {@html renderMarkdown(insights.selectedItem.content)}
            </div>
          {:else}
            <p>{m.insights_page_select_to_read()}</p>
          {/if}
        </article>
      </Card>
    </div>
  {/if}
</section>

<style>
  .generated-insights-panel {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    padding: 1rem 1.5rem 1.5rem;
  }

  .generated-heading h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: 1rem;
  }

  .generated-heading p {
    margin: 0.25rem 0 0;
    color: var(--text-secondary);
    font-size: 0.82rem;
  }

  .scope-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.85rem;
    padding: 1rem;
  }

  .scope-field {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: 0.35rem;
    color: var(--text-secondary);
    font-size: 0.72rem;
    font-weight: 600;
  }

  .focus-field {
    grid-column: span 2;
  }

  .focus-field textarea {
    min-height: 3.75rem;
    resize: vertical;
    border: 1px solid var(--border-primary);
    border-radius: 6px;
    background: var(--bg-secondary);
    color: var(--text-primary);
    font: inherit;
    font-weight: 400;
    padding: 0.55rem 0.65rem;
  }

  :global(.scope-grid .generate-action) {
    align-self: end;
  }

  :global(.archive-state) {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    color: var(--text-secondary);
  }

  .generated-layout {
    display: grid;
    grid-template-columns: minmax(14rem, 0.32fr) minmax(0, 1fr);
    min-height: 28rem;
    gap: 1rem;
  }

  .generated-list {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }

  .generated-list button {
    display: flex;
    align-items: flex-start;
    flex-direction: column;
    gap: 0.2rem;
    width: 100%;
    border: 1px solid var(--border-primary);
    border-radius: 6px;
    background: var(--bg-secondary);
    color: var(--text-primary);
    padding: 0.7rem 0.8rem;
    text-align: left;
  }

  .generated-list button:hover,
  .generated-list button.selected {
    border-color: var(--accent-blue);
    background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-secondary));
  }

  .generated-list button span,
  .generated-list button em {
    color: var(--text-secondary);
    font-size: 0.72rem;
    font-style: normal;
  }

  .generated-list button.error-task {
    border-color: var(--accent-red);
  }

  :global(.generated-detail > .kit-card__body) {
    height: 100%;
  }

  :global(.generated-detail) article {
    min-height: 100%;
    padding: 1rem 1.15rem;
  }

  .generated-detail-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 1rem;
  }

  .generated-meta,
  .generated-actions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.45rem;
  }

  .generated-badge {
    border-radius: 999px;
    background: color-mix(in srgb, var(--accent-purple) 16%, transparent);
    color: var(--accent-purple);
    font-size: 0.7rem;
    font-weight: 700;
    padding: 0.2rem 0.5rem;
  }

  .generated-meta-chip {
    display: inline-flex;
    align-items: center;
    min-height: 18px;
    padding: 2px 6px;
    border: 1px solid var(--border-muted);
    border-radius: 3px;
    background: var(--bg-inset);
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.02em;
    text-transform: uppercase;
  }

  .markdown-body {
    color: var(--text-primary);
    line-height: 1.55;
  }

  @media (max-width: 900px) {
    .scope-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }

    .generated-layout {
      grid-template-columns: 1fr;
    }
  }

  @media (max-width: 640px) {
    .generated-insights-panel {
      padding: 0.85rem 1rem 1rem;
    }

    .scope-grid {
      grid-template-columns: 1fr;
    }

    .focus-field {
      grid-column: auto;
    }

    .generated-detail-head {
      flex-direction: column;
    }
  }
</style>
