<script lang="ts">
  import {
    Button,
    FlashBanner,
    SearchInput,
    SettingsLayout,
    SettingsSection,
    showFlash,
    TextInput,
    type SettingsCategory,
  } from "@kenn-io/kit-ui";
  import { onMount, untrack } from "svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { setAuthToken, getAuthToken, setServerUrl, isRemoteConnection } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import AppearanceSettings from "./AppearanceSettings.svelte";
  import AgentDirSettings from "./AgentDirSettings.svelte";
  import DateRangeSettings from "./DateRangeSettings.svelte";
  import TerminalSettings from "./TerminalSettings.svelte";
  import EmbeddingsSettings from "./EmbeddingsSettings.svelte";
  import GithubSettings from "./GithubSettings.svelte";
  import LanguageSettings from "./LanguageSettings.svelte";
  import RemoteSettings from "./RemoteSettings.svelte";
  import { settingsPanels } from "./settingsPanels.js";

  let authTokenInput: string = $state("");
  let active = $state("appearance");
  let searchQuery = $state("");
  let pageElement: HTMLElement;
  const panels = $derived(settingsPanels());

  function normalizeSearchText(value: string): string {
    return value
      .normalize("NFD")
      .replace(/[\u0300-\u036f]/g, "")
      .toLowerCase();
  }

  const categories: SettingsCategory[] = $derived.by(() => {
    const query = normalizeSearchText(searchQuery.trim());
    return panels
      .filter(
        (panel) =>
          query === "" ||
          normalizeSearchText(
            `${panel.label} ${panel.title} ${panel.group} ${panel.description} ${panel.keywords}`,
          ).includes(query),
      )
      .map((panel) => ({
        id: panel.id,
        label: panel.label,
        group: panel.group,
        summary: panel.description,
      }));
  });
  const layoutCategories: SettingsCategory[] = $derived.by(() => {
    if (categories.length > 0) return categories;
    const activePanel = panels.find((panel) => panel.id === active);
    return activePanel
      ? [{
          id: activePanel.id,
          label: activePanel.label,
          group: activePanel.group,
          summary: activePanel.description,
        }]
      : [];
  });
  const displayedActive = $derived(
    categories.some((category) => category.id === active)
      ? active
      : (categories[0]?.id ?? active),
  );
  const noSearchResults = $derived(
    searchQuery.trim() !== "" && categories.length === 0,
  );
  const searchStatus = $derived.by(() => {
    if (searchQuery.trim() === "") return "";
    if (noSearchResults) return m.settings_search_empty();
    const activePanel = panels.find((panel) => panel.id === displayedActive);
    return activePanel
      ? m.settings_search_result({ category: activePanel.label })
      : "";
  });

  $effect(() => {
    displayedActive;
    const scroller = pageElement?.querySelector<HTMLElement>(
      ".kit-settings__scroll",
    );
    if (scroller) scroller.scrollTop = 0;
  });

  $effect(() => {
    const saveError = settings.saveError;
    if (saveError) {
      untrack(() => showFlash(saveError, { tone: "danger" }));
    }
  });

  onMount(() => {
    authTokenInput = getAuthToken();
    settings.load();
  });

  function handleAuthSubmit() {
    const token = authTokenInput.trim();
    if (!token) return;
    setAuthToken(token);
    window.location.reload();
  }
</script>

<div
  class="settings-page"
  class:settings-no-results={noSearchResults}
  bind:this={pageElement}
>
  <FlashBanner toneLabels={{ danger: m.settings_save_error_label() }} />
  {#if settings.loading || !settings.loaded || settings.needsAuth || settings.error}
    <div class="settings-standalone">
      <div class="settings-header">
        <h2 class="settings-title">{m.settings_title()}</h2>
      </div>

      {#if settings.loading || !settings.loaded}
        <div class="settings-loading">{m.settings_loading()}</div>
      {:else if settings.needsAuth}
        <div class="auth-prompt">
          <h3 class="auth-title">{m.app_auth_title()}</h3>
          <p class="auth-description">
            {m.app_auth_description()}
          </p>
          <div class="auth-field">
            <TextInput
              class="auth-input"
              size="md"
              type="password"
              placeholder={m.app_auth_placeholder()}
              bind:value={authTokenInput}
              onkeydown={(e) => { if (e.key === "Enter") handleAuthSubmit(); }}
            />
            <button
              class="auth-btn"
              disabled={!authTokenInput.trim()}
              onclick={handleAuthSubmit}
            >
              {m.app_auth_authenticate()}
            </button>
          </div>
          <button
            class="auth-disconnect"
            onclick={() => {
              setAuthToken("");
              setServerUrl("");
              settings.needsAuth = false;
              settings.load();
            }}
          >
            {m.app_auth_disconnect_reset()}
          </button>
        </div>
      {:else if settings.error}
        <div class="settings-error">
          <p>{settings.error}</p>
          {#if isRemoteConnection()}
            <button
              class="auth-disconnect"
              onclick={() => {
                setAuthToken("");
                setServerUrl("");
                window.location.reload();
              }}
            >
              {m.app_auth_disconnect_reset()}
            </button>
          {/if}
        </div>
      {/if}
    </div>
  {:else}
    <SettingsLayout categories={layoutCategories} bind:active title={m.settings_title()}>
      {#snippet sidebarHeader()}
        <SearchInput
          bind:value={searchQuery}
          placeholder={m.settings_search_placeholder()}
          ariaLabel={m.settings_search_aria()}
          clearLabel={m.settings_search_clear()}
          size="sm"
          block
        />
        <p
          class="settings-search-status"
          class:settings-search-empty={noSearchResults}
          role="status"
          aria-live="polite"
          aria-atomic="true"
        >
          {searchStatus}
        </p>
      {/snippet}
      {#snippet panel(activeId)}
        {#each panels as meta (meta.id)}
          <div class="settings-panel" hidden={meta.id !== activeId}>
            <SettingsSection title={meta.title} description={meta.description}>
              {#if meta.id === "appearance"}
                <AppearanceSettings />
              {:else if meta.id === "language"}
                <LanguageSettings />
              {:else if meta.id === "date-ranges"}
                <DateRangeSettings />
              {:else if meta.id === "terminal"}
                <TerminalSettings />
              {:else if meta.id === "agent-directories"}
                <AgentDirSettings />
              {:else if meta.id === "worktree-mappings"}
                <Button
                  label={m.settings_worktree_moved_link()}
                  onclick={() => router.navigate("data", { view: "rules" })}
                />
              {:else if meta.id === "embeddings"}
                <EmbeddingsSettings />
              {:else if meta.id === "github"}
                <GithubSettings />
              {:else if meta.id === "remote-access"}
                <RemoteSettings />
              {/if}
            </SettingsSection>
          </div>
        {/each}
      {/snippet}
      {#snippet footer()}
        <Button
          onclick={() => {
            if (!sync.readOnly) ui.activeModal = "resync";
          }}
          disabled={sync.readOnly}
          title={sync.readOnly
            ? m.settings_resync_title_unavailable()
            : m.resync_title()}
        >
          {m.resync_title()}
        </Button>
        <span class="settings-actions-hint">
          {sync.readOnly
            ? m.settings_resync_unavailable_hint()
            : m.settings_resync_hint()}
        </span>
      {/snippet}
    </SettingsLayout>
  {/if}
</div>

<style>
  .settings-page {
    display: flex;
    flex: 1 1 auto;
    min-height: 0;
    width: 100%;
  }

  .settings-standalone {
    width: 100%;
    max-width: 640px;
    max-height: 100%;
    min-height: 0;
    margin: 0 auto;
    padding: 24px 20px 48px;
    overflow-y: auto;
  }

  .settings-header {
    margin-bottom: 20px;
  }

  .settings-title {
    font-size: 18px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .settings-panel[hidden] {
    display: none;
  }

  .settings-page.settings-no-results :global(.kit-settings__nav),
  .settings-page.settings-no-results :global(.kit-settings__panel) {
    display: none;
  }

  .settings-search-status,
  .settings-search-empty {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .settings-loading,
  .settings-error {
    font-size: 13px;
    color: var(--text-muted);
    padding: 40px 0;
    text-align: center;
  }

  .settings-error {
    color: var(--accent-red, #ef4444);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
  }

  .settings-error p {
    margin: 0;
  }

  .settings-actions-hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .auth-prompt {
    text-align: center;
    padding: 40px 20px;
  }

  .auth-title {
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0 0 8px;
  }

  .auth-description {
    font-size: 13px;
    color: var(--text-muted);
    margin: 0 0 20px;
    max-width: 400px;
    margin-left: auto;
    margin-right: auto;
  }

  .auth-field {
    display: flex;
    gap: 8px;
    justify-content: center;
    max-width: 400px;
    margin: 0 auto;
  }

  :global(.auth-input.kit-text-input) {
    flex: 1;
    height: 34px;
    font-family: var(--font-mono, monospace);
  }

  .auth-btn {
    height: 34px;
    padding: 0 16px;
    border-radius: var(--radius-sm);
    font-size: 13px;
    font-weight: 500;
    color: var(--accent-blue-foreground);
    background: var(--accent-blue);
    border: none;
    cursor: pointer;
    white-space: nowrap;
  }

  .auth-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }

  .auth-btn:hover:not(:disabled) {
    opacity: 0.9;
  }

  .auth-disconnect {
    margin-top: 12px;
    background: none;
    border: none;
    color: var(--text-muted);
    font-size: 12px;
    cursor: pointer;
    text-decoration: underline;
  }

  .auth-disconnect:hover {
    color: var(--text-secondary);
  }
</style>
