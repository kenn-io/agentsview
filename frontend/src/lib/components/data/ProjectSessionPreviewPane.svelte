<script lang="ts">
  import { Button, IconButton, showFlash } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    DataService,
    SessionsService,
    SettingsService,
    type DbSession,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { ChevronDownIcon, ChevronLeftIcon, ChevronRightIcon, FunnelIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { data } from "../../stores/data.svelte.js";
  import type { Message } from "../../api/types.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import MessageContent from "../content/MessageContent.svelte";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";

  interface Props {
    projectKey: string;
    projects: ProjectInfo[];
    readOnly: boolean;
    onAssigned: (target: string) => Promise<boolean>;
  }

  let { projectKey, projects, readOnly, onAssigned }: Props = $props();

  let sessions = $state<DbSession[]>([]);
  let expanded = $state(true);
  let total = $state(0);
  let nextCursor = $state<string | undefined>();
  let loadingMore = $state(false);
  let activeIndex = $state(0);
  let loading = $state(true);
  let loadError = $state("");
  let messagesBySession = $state<Record<string, Message[]>>({});
  let messagesLoadingId = $state("");
  let messagesError = $state("");
  let targetProject = $state("");
  let assigning = $state(false);
  let assignmentError = $state("");
  let assignmentRefreshError = $state("");
  const sessionsRead = new LatestRead();
  const messagesRead = new LatestRead();

  const activeSession = $derived(sessions[activeIndex]);
  const activeMessages = $derived(
    activeSession ? (messagesBySession[activeSession.id] ?? []) : [],
  );

  onMount(() => void loadSessions());
  onDestroy(() => {
    sessionsRead.cancel();
    messagesRead.cancel();
  });

  async function loadSessions(append = false): Promise<boolean> {
    const signal = sessionsRead.begin();
    if (append) loadingMore = true;
    else loading = true;
    loadError = "";
    try {
      const response = await callGenerated(
        (options) => DataService.getApiV1DataProjectsByProjectKeySessions({
          projectKey,
        }, { ...data.dateParams, cursor: append ? nextCursor : undefined, limit: 20, include_automated: data.includeAutomatedPreviews }, options),
        signal,
      );
      if (!sessionsRead.isCurrent(signal)) return false;
      const page = (response.sessions ?? []) as DbSession[];
      sessions = append ? [...sessions, ...page] : page;
      total = response.total;
      nextCursor = response.next_cursor;
      if (!append) {
        activeIndex = 0;
        targetProject = "";
        const firstSession = sessions[0];
        if (firstSession) void loadMessages(firstSession.id);
      }
      return true;
    } catch (error) {
      if (isAbortError(error) || !sessionsRead.isCurrent(signal)) return false;
      loadError = m.data_reclassify_session_preview_failed();
      return false;
    } finally {
      if (sessionsRead.finish(signal)) {
        loading = false;
        loadingMore = false;
      }
    }
  }

  async function move(offset: number) {
    if (loadingMore || loading || assigning) return;
    if (offset > 0 && activeIndex === sessions.length - 1 && nextCursor) {
      if (!await loadSessions(true)) return;
    }
    const nextIndex = Math.max(0, Math.min(sessions.length - 1, activeIndex + offset));
    if (nextIndex === activeIndex) return;
    activeIndex = nextIndex;
    targetProject = "";
    assignmentError = "";
    const nextSession = sessions[nextIndex];
    if (nextSession) void loadMessages(nextSession.id);
  }

  async function assignActiveSession() {
    const session = activeSession;
    const target = targetProject.trim();
    if (!session || !target || assigning) return;
    assigning = true;
    assignmentError = "";
    assignmentRefreshError = "";
    try {
      const assignment = await callGenerated(() =>
        SettingsService.putApiV1SettingsSessionProjectAssignmentsBySessionId({
          sessionId: session.id,
        }, { project: target }),
      );
      let inventoryRefreshed = false;
      try {
        inventoryRefreshed = await onAssigned(assignment.project);
      } catch {
        inventoryRefreshed = false;
      }
      targetProject = "";
      await loadSessions();
      if (!inventoryRefreshed) {
        assignmentRefreshError = m.data_session_assignment_refresh_failed();
      }
      showFlash(m.data_session_assignment_saved({ project: assignment.project }), {
        tone: "success",
      });
    } catch (error) {
      assignmentError = error instanceof Error
        ? error.message
        : m.data_session_assignment_failed();
    } finally {
      assigning = false;
    }
  }

  async function clearActiveAssignment() {
    const session = activeSession;
    if (!session || !session.project_assigned || assigning) return;
    assigning = true;
    assignmentError = "";
    assignmentRefreshError = "";
    try {
      const cleared = await callGenerated(() =>
        SettingsService.deleteApiV1SettingsSessionProjectAssignmentsBySessionId({
          sessionId: session.id,
        }),
      );
      let inventoryRefreshed = false;
      try {
        inventoryRefreshed = await onAssigned(cleared.project);
      } catch {
        inventoryRefreshed = false;
      }
      await loadSessions();
      if (!inventoryRefreshed) {
        assignmentRefreshError = m.data_session_assignment_refresh_failed();
      }
      showFlash(m.data_session_assignment_cleared({ project: cleared.project }), {
        tone: "success",
      });
    } catch (error) {
      assignmentError = error instanceof Error
        ? error.message
        : m.data_session_assignment_clear_failed();
    } finally {
      assigning = false;
    }
  }

  async function loadMessages(sessionId: string) {
    messagesError = "";
    messagesRead.cancel();
    messagesLoadingId = "";
    if (messagesBySession[sessionId]) return;
    const signal = messagesRead.begin();
    messagesLoadingId = sessionId;
    try {
      const response = await callGenerated(
        (options) => SessionsService.getApiV1SessionsByIdMessages({
          id: sessionId,
        }, {
          limit: 12,
          direction: "asc",
          roles: "user,assistant",
        }, options),
        signal,
      );
      if (!messagesRead.isCurrent(signal)) return;
      // The generated message list has the same temporary `any[]` boundary
      // as the session list above.
      messagesBySession = {
        ...messagesBySession,
        [sessionId]: (response.messages ?? []) as Message[],
      };
    } catch (error) {
      if (isAbortError(error) || !messagesRead.isCurrent(signal)) return;
      messagesError = m.data_reclassify_session_preview_failed();
    } finally {
      if (messagesRead.finish(signal)) messagesLoadingId = "";
    }
  }
</script>

<section class="project-session-previews">
  <div class="preview-header">
    <Button
      size="sm"
      class="session-preview-toggle"
      label={m.data_reclassify_session_preview_count({ count: total })}
      ariaExpanded={expanded}
      onclick={() => (expanded = !expanded)}
    >
      {#snippet trailing()}
        {#if expanded}
          <ChevronDownIcon size="13" strokeWidth="2.2" aria-hidden="true" />
        {:else}
          <ChevronRightIcon size="13" strokeWidth="2.2" aria-hidden="true" />
        {/if}
      {/snippet}
    </Button>

    {#if expanded && activeSession}
      <div class="carousel-nav">
        <span>{m.data_reclassify_session_preview_position({ current: activeIndex + 1, count: total })}</span>
        <div class="carousel-buttons">
          <IconButton
            size="sm"
            ariaLabel={m.data_reclassify_session_preview_previous()}
            disabled={assigning || loading || loadingMore || activeIndex === 0}
            onclick={() => move(-1)}
          >
            <ChevronLeftIcon size="14" aria-hidden="true" />
          </IconButton>
          <IconButton
            size="sm"
            ariaLabel={m.data_reclassify_session_preview_next()}
            disabled={assigning || loading || loadingMore || (activeIndex === sessions.length - 1 && !nextCursor)}
            onclick={() => move(1)}
          >
            <ChevronRightIcon size="14" aria-hidden="true" />
          </IconButton>
        </div>
      </div>
    {/if}
    <IconButton
      size="sm"
      ariaLabel={m.sidebar_filters_include_automated()}
      title={m.sidebar_filters_include_automated()}
      ariaPressed={data.includeAutomatedPreviews}
      tone={data.includeAutomatedPreviews ? "info" : "neutral"}
      disabled={assigning || loading || loadingMore}
      onclick={() => {
        data.includeAutomatedPreviews = !data.includeAutomatedPreviews;
        void loadSessions();
      }}
    >
      <FunnelIcon size="14" aria-hidden="true" />
    </IconButton>
  </div>

  {#if loading}
    <p class="preview-status">{m.data_reclassify_session_preview_loading()}</p>
  {:else if loadError}
    <p class="preview-status error-text">{loadError}</p>
  {:else if expanded && !activeSession}
    <p class="preview-status">{data.includeAutomatedPreviews
      ? m.data_reclassify_session_preview_no_message()
      : m.data_reclassify_session_preview_filtered_empty()}</p>
  {:else if expanded && activeSession}
    <div class="carousel" aria-live="polite">
      <div class="session-transcript">
        {#if messagesLoadingId === activeSession.id}
          <p class="preview-status">{m.data_reclassify_session_preview_loading()}</p>
        {:else if messagesError}
          <p class="preview-status error-text">{messagesError}</p>
        {:else if activeMessages.length === 0}
          <p class="preview-status">{m.data_reclassify_session_preview_no_message()}</p>
        {:else}
          {#each activeMessages as message (message.id)}
            <div class="preview-message">
              <MessageContent
                {message}
                session={activeSession}
                compact
                allowMutations={false}
              />
            </div>
          {/each}
        {/if}
      </div>

      {#if activeSession.cwd}
        <div class="session-folder">
          <span>{m.data_reclassify_session_preview_folder()}</span>
          <code>{activeSession.cwd}</code>
        </div>
      {/if}

      <div class="session-assignment">
        <div class="assignment-copy">
          <div class="assignment-heading">
            <strong>{m.data_session_assignment_heading()}</strong>
            <span class:manual={activeSession.project_assigned} class="assignment-status">
              {activeSession.project_assigned
                ? m.data_session_assignment_manual()
                : m.data_session_assignment_automatic()}
            </span>
          </div>
          <span>{m.data_session_assignment_intro()}</span>
        </div>
        {#if readOnly}
          <p class="preview-status">{m.data_reclassify_read_only()}</p>
        {:else}
          <div class="assignment-controls">
            <ProjectTypeahead
              {projects}
              value={targetProject}
              onselect={(value) => (targetProject = value)}
              onquery={() => (assignmentError = "")}
              includeAll={false}
              allowCustom={true}
              customLabel={m.data_reclassify_use_custom_project({ query: "{query}" })}
              placeholder={m.data_session_assignment_target()}
              title={m.data_session_assignment_target()}
            />
            <Button
              size="sm"
              label={assigning
                ? m.data_session_assignment_saving()
                : m.data_session_assignment_save()}
              disabled={!targetProject.trim() || assigning}
              onclick={() => void assignActiveSession()}
            />
            {#if activeSession.project_assigned}
              <Button
                size="sm"
                label={assigning
                  ? m.data_session_assignment_clearing()
                  : m.data_session_assignment_use_automatic()}
                disabled={assigning}
                onclick={() => void clearActiveAssignment()}
              />
            {/if}
          </div>
          {#if assignmentError}
            <p class="preview-status error-text" role="alert">{assignmentError}</p>
          {/if}
        {/if}
      </div>
    </div>
  {/if}
  {#if assignmentRefreshError}
    <p class="preview-status error-text" role="status">
      {assignmentRefreshError}
    </p>
  {/if}
</section>

<style>
  .project-session-previews {
    display: flex;
    flex: 1;
    min-height: 0;
    flex-direction: column;
    gap: 8px;
  }

  .project-session-previews :global(.session-preview-toggle.kit-button) {
    flex: 1;
    min-width: 0;
    min-height: 28px;
    justify-content: space-between;
    padding: 0 2px;
    border: 0;
    border-radius: 0;
    background: transparent;
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 650;
    transform: none;
  }

  .project-session-previews :global(.session-preview-toggle.kit-button:hover:not(:disabled)) {
    border-color: transparent;
    background: transparent;
    color: var(--text-primary);
  }

  .carousel {
    display: flex;
    flex: 1;
    min-height: 0;
    flex-direction: column;
    gap: 8px;
  }

  .preview-header,
  .carousel-nav,
  .carousel-buttons {
    display: flex;
    align-items: center;
  }

  .preview-header { gap: 8px; }

  .carousel-nav {
    flex: none;
    gap: 8px;
    min-height: 24px;
    justify-content: space-between;
    color: var(--text-muted);
    font-size: 10px;
  }

  .carousel-buttons {
    gap: 2px;
  }

  .session-transcript {
    flex: 1;
    min-height: 120px;
    overflow-y: auto;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .preview-message {
    padding: 5px 8px;
  }

  .preview-status {
    min-height: 32px;
    margin: 0;
    padding: 8px 2px;
    color: var(--text-muted);
    font-size: 11px;
  }

  .error-text {
    color: var(--accent-red);
  }

  .session-folder {
    display: flex;
    min-width: 0;
    flex-direction: column;
    gap: 4px;
  }

  .session-folder span {
    color: var(--text-muted);
    font-size: 9px;
    text-transform: uppercase;
  }

  .session-folder code {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    overflow-wrap: anywhere;
  }

  .session-assignment {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding-top: 10px;
    border-top: 1px solid var(--border-muted);
  }

  .assignment-copy {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .assignment-heading {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .assignment-copy strong {
    color: var(--text-primary);
    font-size: 11px;
  }

  .assignment-copy span {
    color: var(--text-muted);
    font-size: 10px;
    line-height: 1.4;
  }

  .assignment-copy .assignment-status {
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 550;
  }

  .assignment-copy .assignment-status.manual {
    color: var(--accent-blue);
  }

  .assignment-controls {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto auto;
    align-items: center;
    gap: 8px;
  }
</style>
