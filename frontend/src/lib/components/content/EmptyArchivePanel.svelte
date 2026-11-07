<script lang="ts">
  import { Button, EmptyState } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { FolderIcon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import SidebarToggleButton from "../layout/SidebarToggleButton.svelte";

  const folders = $derived(
    settings.sessionProviders
      .filter((provider) => !settings.disabledAgents.includes(provider.id))
      .flatMap((provider) =>
        provider.dirs.map((dir) => ({ name: provider.display_name, dir })),
      ),
  );
</script>

<div class="empty-archive-panel">
  {#if !ui.isMobileViewport && !ui.sidebarOpen}
    <div class="empty-archive-toolbar" data-sidebar-focus-region="content">
      <SidebarToggleButton placement="content" />
    </div>
  {/if}
  <EmptyState title={m.empty_archive_title()} description={m.empty_archive_desc()}>
    {#snippet icon()}
      <FolderIcon size="40" strokeWidth="1.6" aria-hidden="true" />
    {/snippet}
    {#if settings.loaded && !settings.error}
      {#if folders.length === 0}
        <p class="empty-archive-folders empty-archive-none">
          {m.settings_session_providers_not_configured()}
        </p>
      {:else}
        <ul class="empty-archive-folders">
          {#each folders as folder}
            <li>{folder.name}: <code>{folder.dir}</code></li>
          {/each}
        </ul>
      {/if}
    {/if}
    <Button
      class="empty-archive-choose"
      onclick={() => router.navigate("settings", { panel: "agent-directories" })}
    >
      {m.empty_archive_choose_folders()}
    </Button>
  </EmptyState>
</div>

<style>
  .empty-archive-panel {
    display: flex;
    flex-direction: column;
    height: 100%;
    overflow-y: auto;
  }

  .empty-archive-toolbar {
    display: flex;
    align-items: center;
    padding: 8px 16px;
    flex-shrink: 0;
  }

  .empty-archive-folders {
    flex-basis: 100%;
    margin: 0;
    padding: 0;
    color: var(--text-muted);
  }

  /* Content-sized column, centred, so rows share a left edge on the panel axis. */
  ul.empty-archive-folders {
    display: grid;
    justify-content: center;
    gap: var(--space-1);
    max-height: 240px;
    overflow-y: auto;
    list-style: none;
    text-align: left;
  }

  .empty-archive-none,
  .empty-archive-folders code {
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }

  .empty-archive-panel :global(.empty-archive-choose) {
    margin-top: var(--space-3);
  }
</style>
