<script lang="ts">
  import {
    Button,
    SearchInput,
    SettingsLayout,
    SettingsSection,
    TextInput,
    type SettingsCategory,
  } from "@kenn-io/kit-ui";
  import { onMount } from "svelte";
  import { settings } from "../../stores/settings.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { setAuthToken, getAuthToken, setServerUrl, isRemoteConnection } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import AppearanceSettings from "./AppearanceSettings.svelte";
  import AgentDirSettings from "./AgentDirSettings.svelte";
  import DateRangeSettings from "./DateRangeSettings.svelte";
  import TerminalSettings from "./TerminalSettings.svelte";
  import ArchiveContentSettings from "./ArchiveContentSettings.svelte";
  import EmbeddingsSettings from "./EmbeddingsSettings.svelte";
  import ToolImageCleanup from "./ToolImageCleanup.svelte";
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
        icon: panel.icon,
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
    void displayedActive;
    const scroller = pageElement?.querySelector<HTMLElement>(
      ".kit-settings__scroll",
    );
    if (scroller) scroller.scrollTop = 0;
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
              {:else if meta.id === "tool-result-images"}
                <ToolImageCleanup />
              {:else if meta.id === "embeddings"}
                <EmbeddingsSettings />
              {:else if meta.id === "archive-content"}
                <ArchiveContentSettings />
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
        {#if !sync.archiveOnly}
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
        {/if}
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
    /* Buttons (panel and footer) use the same 12px text as the segmented
       options and inputs beside them. */
    --kit-control-font-size: 12px;
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

  /* Reserve the scrollbar's width up front so the centered panel does not
     shift when switching to a panel tall enough to scroll. */
  .settings-page :global(.kit-settings__scroll) {
    scrollbar-gutter: stable;
  }

  /* Local overrides of kit-ui SettingsLayout: readable nav summaries and
     the app-wide neutral selected state. */
  @media (min-width: 761px) {
    .settings-page :global(.kit-settings .kit-settings__sidebar) {
      width: 236px;
    }

    /* Tight vertical rhythm so every category, Remote access included,
       fits a 900px-tall window without scrolling the nav. The small end
       padding keeps the last item clear of the status bar when a shorter
       window does scroll. Narrow widths show a horizontal strip instead. */
    .settings-page :global(.kit-settings .kit-settings__sidebar) {
      padding-block: var(--space-4) var(--space-2);
      gap: var(--space-4);
    }

    /* A few pixels between items and between each name and its
       description separate the entries; the tighter group-title margin and
       description line height pay for them so the nav still fits. */
    .settings-page :global(.kit-settings .kit-settings__nav) {
      gap: var(--space-1);
      padding-bottom: var(--space-2);
    }

    .settings-page :global(.kit-settings .kit-settings__nav-item) {
      padding-block: var(--space-2);
    }

    .settings-page :global(.kit-settings .kit-settings__nav-text) {
      gap: var(--space-2);
    }

    .settings-page :global(.kit-settings .kit-settings__group-title) {
      padding-top: var(--space-3);
      padding-bottom: var(--space-1);
    }

    .settings-page :global(.kit-settings .kit-settings__group-title:not(:first-child)) {
      margin-top: var(--space-1);
    }
  }

  /* Descriptions stay clearly below the item names. The app's light muted
     ink sits close to secondary, so the nav steps it toward the surface;
     both themes keep about 4.5:1 contrast. */
  .settings-page :global(.kit-settings .kit-settings__nav-summary) {
    font-size: 11px;
    font-weight: 400;
    line-height: 1.2;
    color: color-mix(in srgb, var(--text-muted) 85%, var(--bg-surface));
  }

  .settings-page :global(.kit-settings__nav-item) {
    font-weight: 500;
  }

  .settings-page :global(.kit-settings .kit-settings__footer) {
    padding-block: var(--space-3);
  }

  /* The footer action sits on the same 28px control height as the panel. */
  .settings-page :global(.kit-settings__footer) {
    --kit-control-height: 28px;
  }

  /* Light theme leaves --bg-raised unset, so kit-ui buttons and select
     triggers fall back to the gray inset fill and read as disabled. Here
     enabled controls take the surface fill with their border instead;
     dark keeps its lighter raised fill. */
  :global(:root:not(.dark)) .settings-page {
    --bg-raised: var(--bg-surface);
  }

  /* One row grid for every panel: a label column, then the control.
     Panels keep their own markup; this replaces their per-panel
     space-between rows so controls start on a shared edge everywhere.
     Rows with a single child (checkboxes, notes) span both columns. */
  .settings-page :global(.kit-settings__panel) {
    /* Panels with their own row grids (providers, remote access, tool-result
       images) read the same label width. */
    --setting-label-width: 148px;
    /* One control height for every settings control: buttons, inputs, and
       typeahead triggers read this through kit-ui. */
    --kit-control-height: 28px;
    --typeahead-control-height: 28px;
  }

  .settings-page :global(.kit-settings__panel .setting-row:not(.column)) {
    display: grid;
    grid-template-columns: var(--setting-label-width) minmax(0, 1fr);
    align-items: center;
    justify-items: start;
    gap: 12px;
  }

  .settings-page :global(.kit-settings__panel .setting-row:not(.column) > :only-child) {
    grid-column: 1 / -1;
  }

  /* Every panel's field labels share one size, weight, and ink, so no
     panel reads bolder than another. Long translated labels wrap inside
     the label column instead of running under the control. */
  .settings-page :global(.kit-settings__panel .setting-row:not(.column) > .setting-label) {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: normal;
    overflow-wrap: anywhere;
  }

  /* Phones: one column, label above control, and option rows let their
     segmented controls take the full width. */
  @media (max-width: 640px) {
    .settings-page :global(.kit-settings__panel .setting-row:not(.column)) {
      grid-template-columns: minmax(0, 1fr);
      gap: var(--space-2);
    }

    .settings-page :global(.kit-settings__panel .setting-row.option-row) {
      justify-items: stretch;
    }

    .settings-page :global(.kit-settings__panel .setting-row.option-row > .setting-label) {
      justify-self: start;
    }
  }

  /* Segments sit inside the track's 2px padding, so the track itself
     lands on the shared control height. */
  .settings-page :global(.kit-settings__panel .kit-segmented:not(.kit-segmented--borderless) .kit-segmented__btn) {
    min-height: calc(var(--kit-control-height) - 4px);
  }

  /* Disabled and read-only controls share app.css's disabled ink: clearly
     fainter than enabled controls, without the uneven look of opacity. */
  .settings-page :global(.kit-settings__panel button:disabled:not(.kit-button--solid.kit-button--info)),
  .settings-page :global(.kit-settings__footer .kit-button:disabled:not(.kit-button--solid.kit-button--info)),
  .settings-page :global(.kit-settings__panel .kit-segmented__btn:disabled),
  .settings-page :global(.kit-settings__panel .kit-toggle--disabled),
  .settings-page :global(.kit-settings__panel .kit-text-input--disabled) {
    opacity: 1;
    color: var(--disabled-ink);
    cursor: not-allowed;
  }

  /* Inputs keep the surface fill whether or not they are editable; a
     disabled one shows its state through the faint edge and muted text,
     the same no-fill disabled look as the buttons, instead of a gray well
     that made every field on a read-only server look broken. */
  .settings-page :global(.kit-settings__panel .kit-text-input--disabled) {
    background: var(--bg-surface);
    border-color: var(--disabled-edge);
  }

  .settings-page :global(.kit-settings__panel .kit-text-input--disabled .kit-text-input__control) {
    color: var(--text-muted);
  }

  /* Every other disabled button, destructive and footer included, matches
     the disabled primary in app.css: faint edge, no fill, disabled ink. */
  .settings-page :global(.kit-settings__panel .kit-button:disabled:not(.kit-button--solid.kit-button--info)),
  .settings-page :global(.kit-settings__footer .kit-button:disabled:not(.kit-button--solid.kit-button--info)) {
    background: transparent;
    border-color: var(--disabled-edge);
    box-shadow: none;
    color: var(--disabled-ink);
  }

  /* Dark unselected segments already use muted ink (one step below the
     secondary ink of other enabled controls), so the shared disabled ink
     sat too close to them. Disabled segments take one more step down, the
     same muted share light's disabled ink uses. */
  :global(:root.dark) .settings-page :global(.kit-settings__panel .kit-segmented__btn:disabled:not(.active)) {
    color: color-mix(in srgb, var(--text-muted) 58%, var(--bg-surface));
  }

  /* The selected segment of a disabled control keeps app.css's selected
     fill, edge, and weight, and clearly stronger ink than its disabled
     neighbours, so the current choice reads on a read-only server. The
     extra .kit-segmented class outranks the disabled-button rule above,
     whose :not(...) argument adds two classes of specificity. */
  .settings-page :global(.kit-settings__panel .kit-segmented .kit-segmented__btn.active:disabled) {
    color: var(--text-primary);
  }

  /* A disabled switch keeps its on/off state readable, but its track is
     clearly quieter than a live switch so read-only rows do not look
     editable beside their disabled inputs and buttons. The track mixes
     the accent into the surface color in both themes (mixing into the
     border kept dark tracks bright enough to look live), and the knob
     steps down from white. */
  .settings-page :global(.kit-settings__panel .kit-toggle--disabled .kit-toggle__input:checked + .kit-toggle__track) {
    background: color-mix(in srgb, var(--accent-blue) 30%, var(--bg-surface));
  }

  .settings-page :global(.kit-settings__panel .kit-toggle--disabled .kit-toggle__knob) {
    background: color-mix(in srgb, var(--text-muted) 35%, var(--bg-surface));
  }

  :global(:root.dark) .settings-page :global(.kit-settings__panel .kit-toggle--disabled .kit-toggle__knob) {
    background: color-mix(in srgb, var(--text-muted) 70%, var(--bg-surface));
  }

  /* Placeholders sit a clear step below entered values (which use muted
     ink), so example text such as "e.g. ~/.claude-work" never reads as a
     configured value. */
  .settings-page :global(.kit-settings__panel .kit-text-input__control::placeholder) {
    color: color-mix(in srgb, var(--text-muted) 68%, var(--bg-surface));
  }

  /* Panel titles step up from section headings (13px semibold) and row
     labels (12px), so a panel reads as panel > section > row. */
  .settings-page :global(.kit-settings__panel .kit-settings-section__title) {
    font-size: calc(var(--font-size-md) + 2px);
  }

  /* Room below the panel so the sticky footer never clips the preview. */
  .settings-page :global(.kit-settings .kit-settings__panel) {
    padding-bottom: calc(var(--space-8) + var(--space-6));
  }

  .settings-page :global(.kit-settings__nav-item--active),
  .settings-page :global(.kit-settings__nav-item--active:hover) {
    /* A neutral fill and primary ink mark the selected panel; no accent
       edge. */
    background: var(--selected-list-bg);
    color: var(--text-primary);
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
    opacity: var(--opacity-disabled);
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
