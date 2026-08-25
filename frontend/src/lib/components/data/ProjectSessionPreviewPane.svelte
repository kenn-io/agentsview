<script lang="ts">
  import { Button, IconButton, showFlash } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    SessionsService,
    SettingsService,
    type DbSession,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { ChevronDownIcon, ChevronLeftIcon, ChevronRightIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import type { Message } from "../../api/types.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import MessageContent from "../content/MessageContent.svelte";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";

  interface Props {
    projectLabel: string;
    projects: ProjectInfo[];
    readOnly: boolean;
    onAssigned: (target: string) => Promise<boolean>;
  }

  let { projectLabel, projects, readOnly, onAssigned }: Props = $props();

  let sessions = $state<DbSession[]>([]);
  let expanded = $state(true);
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

  async function loadSessions() {
    const signal = sessionsRead.begin();
    loading = true;
    loadError = "";
    try {
      const response = await callGenerated(
        () => SessionsService.getApiV1Sessions({
          project: projectLabel,
          includeOneShot: true,
          includeAutomated: true,
          includeChildren: true,
          limit: 20,
          orderBy: "recent",
        }),
        signal,
      );
      if (!sessionsRead.isCurrent(signal)) return;
      // The generated list model currently exposes `sessions` as `any[]`.
      // Keep the cast at this API boundary and use the generated row model
      // everywhere inside the component.
      const loadedSessions = (response.sessions ?? []) as DbSession[];
      sessions = [
        ...loadedSessions.filter((session) => !session.is_automated),
        ...loadedSessions.filter((session) => session.is_automated),
      ];
      activeIndex = 0;
      const firstSession = sessions[0];
      if (firstSession) await loadMessages(firstSession.id);
    } catch (error) {
      if (isAbortError(error) || !sessionsRead.isCurrent(signal)) return;
      loadError = m.data_reclassify_session_preview_failed();
    } finally {
      if (sessionsRead.finish(signal)) loading = false;
    }
  }

  function move(offset: number) {
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
        SettingsService.putApiV1SettingsSessionProjectAssignmentsSessionId({
          sessionId: session.id,
          requestBody: { project: target },
        }),
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

  async function loadMessages(sessionId: string) {
    messagesError = "";
    messagesRead.cancel();
    messagesLoadingId = "";
    if (messagesBySession[sessionId]) return;
    const signal = messagesRead.begin();
    messagesLoadingId = sessionId;
    try {
      const response = await callGenerated(
        () => SessionsService.getApiV1SessionsIdMessages({
          id: sessionId,
          limit: 12,
          direction: "asc",
          roles: "user,assistant",
        }),
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
  {#if loading}
    <p class="preview-status">{m.data_reclassify_session_preview_loading()}</p>
  {:else if loadError}
    <p class="preview-status error-text">{loadError}</p>
  {:else if sessions.length > 0}
    <Button
      size="sm"
      class="session-preview-toggle"
      label={m.data_reclassify_session_preview_count({ count: sessions.length })}
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
      <div class="carousel" aria-live="polite">
        <div class="carousel-nav">
          <span>{m.data_reclassify_session_preview_position({ current: activeIndex + 1, count: sessions.length })}</span>
          <div class="carousel-buttons">
            <IconButton
              size="sm"
              ariaLabel={m.data_reclassify_session_preview_previous()}
              disabled={assigning || activeIndex === 0}
              onclick={() => move(-1)}
            >
              <ChevronLeftIcon size="14" aria-hidden="true" />
            </IconButton>
            <IconButton
              size="sm"
              ariaLabel={m.data_reclassify_session_preview_next()}
              disabled={assigning || activeIndex === sessions.length - 1}
              onclick={() => move(1)}
            >
              <ChevronRightIcon size="14" aria-hidden="true" />
            </IconButton>
          </div>
        </div>

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
            <strong>{m.data_session_assignment_heading()}</strong>
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
            </div>
            {#if assignmentError}
              <p class="preview-status error-text" role="alert">{assignmentError}</p>
            {/if}
            {#if assignmentRefreshError}
              <p class="preview-status error-text" role="status">
                {assignmentRefreshError}
              </p>
            {/if}
          {/if}
        </div>
      </div>
    {/if}
  {/if}
</section>

<style>
  .project-session-previews {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .project-session-previews :global(.session-preview-toggle.kit-button) {
    width: 100%;
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
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  .carousel-nav,
  .carousel-buttons {
    display: flex;
    align-items: center;
  }

  .carousel-nav {
    min-height: 24px;
    justify-content: space-between;
    color: var(--text-muted);
    font-size: 10px;
  }

  .carousel-buttons {
    gap: 2px;
  }

  .session-transcript {
    min-height: 120px;
    max-height: 320px;
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

  .assignment-copy strong {
    color: var(--text-primary);
    font-size: 11px;
  }

  .assignment-copy span {
    color: var(--text-muted);
    font-size: 10px;
    line-height: 1.4;
  }

  .assignment-controls {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: 8px;
  }
</style>
