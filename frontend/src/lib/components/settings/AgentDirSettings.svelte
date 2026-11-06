<script lang="ts">
  import { Button, IconButton, TextInput, Toggle } from "@kenn-io/kit-ui";
  import { XIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";

  const HOME_ENV_VARS: Record<string, string> = {
    claude: "CLAUDE_CONFIG_DIR",
    codex: "CODEX_HOME",
    pi: "PI_CODING_AGENT_DIR",
  };

  let homeDrafts: Record<string, string> = $state({});
  let homesSaving: string | null = $state(null);

  let pendingDisabledAgents: string[] = $state([]);
  let providerSaving = $state(false);
  let saveFailed = $state(false);
  let changesApplied = $state(false);

  $effect(() => {
    if (!providerSaving) {
      pendingDisabledAgents = [...settings.disabledAgents];
    }
  });

  // Path segments that each end with their separator, so a long path
  // wraps only after "/" or "\\" and never inside a directory name.
  function pathSegments(path: string): string[] {
    return path.split(/(?<=[/\\])/);
  }

  function providerEnabled(id: string): boolean {
    return !pendingDisabledAgents.includes(id);
  }

  async function setProviderEnabled(id: string, enabled: boolean) {
    if (providerSaving || settings.saving || settings.readOnly) return;

    const confirmed = [...settings.disabledAgents];
    const next = enabled
      ? confirmed.filter((agent) => agent !== id)
      : [...confirmed, id];
    pendingDisabledAgents = next;
    providerSaving = true;
    saveFailed = false;

    const saved = await settings.save({ disabled_agents: next });
    if (saved) {
      pendingDisabledAgents = [...settings.disabledAgents];
      changesApplied = true;
    } else {
      pendingDisabledAgents = confirmed;
      saveFailed = true;
    }
    providerSaving = false;
  }

  function busy(): boolean {
    return providerSaving || homesSaving !== null || settings.saving || settings.readOnly;
  }

  async function saveHomes(id: string, next: string[]) {
    if (busy()) return false;
    homesSaving = id;
    saveFailed = false;
    const saved = await settings.save({ agent_homes: { [id]: next } });
    if (saved) {
      changesApplied = true;
    } else {
      saveFailed = true;
    }
    homesSaving = null;
    return saved;
  }

  async function addHome(id: string, current: string[]) {
    const draft = (homeDrafts[id] ?? "").trim();
    if (!draft || current.includes(draft)) return;
    if (await saveHomes(id, [...current, draft])) {
      homeDrafts[id] = "";
    }
  }

  function removeHome(id: string, current: string[], home: string) {
    void saveHomes(
      id,
      current.filter((entry) => entry !== home),
    );
  }
</script>

<div class="provider-list">
  {#each settings.sessionProviders as provider (provider.id)}
    <div class="provider-row">
      <span class="provider-name">{provider.display_name}</span>
      <Toggle
        checked={providerEnabled(provider.id)}
        disabled={busy()}
        ariaLabel={m.settings_session_providers_enable_aria({
          provider: provider.display_name,
        })}
        onchange={(enabled) => setProviderEnabled(provider.id, enabled)}
      />
      <div class="provider-details">
        <div class="provider-paths">
          {#if provider.dirs.length === 0}
            <span class="provider-not-configured">
              {m.settings_session_providers_not_configured()}
            </span>
          {:else}
            {#each provider.dirs as dir}
              <code class="provider-path">{#each pathSegments(dir) as seg, i (i)}<span
                    class="provider-path-seg">{seg}</span><wbr />{/each}</code>
            {/each}
          {/if}
        </div>
        {#if provider.homes_supported}
          <div class="provider-homes">
            <span class="provider-homes-title">
              {m.settings_session_providers_homes_title()}
            </span>
            {#each provider.homes as home (home)}
              <div class="provider-home">
                <code class="provider-path">{#each pathSegments(home) as seg, i (i)}<span
                      class="provider-path-seg">{seg}</span><wbr />{/each}</code>
                <IconButton
                  size="sm"
                  disabled={busy()}
                  ariaLabel={m.settings_session_providers_homes_remove_aria({
                    provider: provider.display_name,
                    home,
                  })}
                  onclick={() => removeHome(provider.id, provider.homes, home)}
                >
                  <XIcon size="12" strokeWidth="2" aria-hidden="true" />
                </IconButton>
              </div>
            {/each}
            <div class="provider-home-add">
              <TextInput
                size="md"
                block
                disabled={busy()}
                placeholder={m.settings_session_providers_homes_placeholder({
                  agent: provider.id,
                })}
                ariaLabel={m.settings_session_providers_homes_input_aria({
                  provider: provider.display_name,
                })}
                value={homeDrafts[provider.id] ?? ""}
                oninput={(value) => {
                  homeDrafts[provider.id] = value;
                }}
                onkeydown={(e) => {
                  if (e.key === "Enter") void addHome(provider.id, provider.homes);
                }}
              />
              <Button
                disabled={busy() || !(homeDrafts[provider.id] ?? "").trim()}
                onclick={() => void addHome(provider.id, provider.homes)}
              >
                {m.settings_session_providers_homes_add()}
              </Button>
            </div>
            <p class="provider-homes-hint">
              {m.settings_session_providers_homes_hint({
                envVar: HOME_ENV_VARS[provider.id] ?? "HOME",
              })}
            </p>
          </div>
        {/if}
      </div>
    </div>
  {/each}

  {#if changesApplied}
    <p class="provider-status" role="status" aria-live="polite">
      {m.settings_session_providers_applied_notice()}
    </p>
  {/if}
  {#if saveFailed}
    <p class="provider-error" role="alert">
      {m.settings_session_providers_save_failed()}
    </p>
  {/if}
</div>

<style>
  /* Three columns: provider name, switch, configuration. The list owns the
     columns and every row is a subgrid of them, so the name column is only
     as wide as the longest provider name (capped at the settings label
     width, where longer names wrap) and the switches and paths still line
     up down the list. This panel trades lining up with the label column of
     the other settings panels for a tighter name column. */
  .provider-list {
    display: grid;
    grid-template-columns: fit-content(var(--setting-label-width, 222px)) auto minmax(0, 1fr);
    column-gap: 12px;
  }

  .provider-list > :not(.provider-row) {
    grid-column: 1 / -1;
  }

  .provider-row {
    display: grid;
    grid-column: 1 / -1;
    grid-template-columns: subgrid;
    align-items: start;
    column-gap: 12px;
    row-gap: var(--space-2);
    padding: var(--space-3) 0;
    border-bottom: 1px solid var(--border-muted);
  }

  .provider-row:first-child {
    padding-top: 0;
  }

  .provider-details {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
    padding-inline-start: var(--space-2);
  }

  /* Provider names use the shared field-label size and weight with
     primary ink, since each one heads a block; the 20px line box matches
     the switch track. */
  .provider-name {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 500;
    line-height: 20px;
    overflow-wrap: anywhere;
  }

  /* The top padding centers the first path line on the 20px line the
     name and switch share. */
  .provider-paths {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding-top: calc((20px - 11px * 1.4) / 2);
  }

  .provider-path,
  .provider-not-configured {
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.4;
    letter-spacing: -0.01em;
  }

  /* Wrap only between segments (after a separator); each segment,
     including names with spaces, stays on one line. */
  /* A path that wraps indents its continuation lines, so a wrapped tail
     never reads as the start of another path. */
  .provider-path {
    min-width: 0;
    padding-inline-start: 2ch;
    text-indent: -2ch;
  }

  /* Each segment is its own inline box, so lines still break between
     segments; a single segment wider than the column wraps inside its
     box instead of running out of the panel. */
  .provider-path-seg {
    display: inline-block;
    max-width: 100%;
    text-indent: 0;
    overflow-wrap: anywhere;
  }

  .provider-not-configured {
    font-style: italic;
  }

  .provider-homes {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin-top: var(--space-2);
    min-width: 0;
  }

  .provider-homes-title {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 400;
  }

  .provider-home {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    min-width: 0;
  }

  .provider-home > .provider-path {
    flex: 1 1 auto;
  }

  /* Phones: like the other settings rows, the label column goes away.
     The name and switch share the header line and the configuration
     takes the row's full width below them. */
  @media (max-width: 640px) {
    .provider-list {
      display: flex;
      flex-direction: column;
    }

    .provider-row {
      grid-template-columns: minmax(0, 1fr) auto;
    }

    .provider-details {
      grid-column: 1 / -1;
      padding-inline-start: 0;
    }

    .provider-paths {
      padding-top: 0;
    }
  }

  /* The Add button moves under the field when a long translated label
     leaves the field too little room beside it. */
  .provider-home-add {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    max-width: 420px;
  }

  .provider-home-add > :global(.kit-text-input) {
    flex: 1 1 180px;
    min-width: 0;
  }

  .provider-homes-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.4;
  }

  .provider-status,
  .provider-error {
    margin: 10px 0 0;
    font-size: 11px;
    line-height: 1.5;
  }

  .provider-status {
    color: var(--text-muted);
  }

  .provider-error {
    color: var(--accent-red);
  }
</style>
