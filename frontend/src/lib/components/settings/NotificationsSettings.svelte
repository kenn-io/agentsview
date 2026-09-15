<script lang="ts">
  import { Toggle, TextInput } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import {
    ConfigService,
    type NotificationsConfigBody,
  } from "../../api/generated/index";

  let enabled: boolean = $state(false);
  let notifyNewReply: boolean = $state(false);
  let suppressSubagents: boolean = $state(true);
  let mergeWindow: string = $state("60");
  let agents: string = $state("");
  let projects: string = $state("");
  let loaded: boolean = $state(false);
  let saving: boolean = $state(false);
  let error: string | null = $state(null);
  let success: string | null = $state(null);

  $effect(() => {
    if (loaded) return;
    loaded = true;
    void reload();
  });

  async function reload() {
    error = null;
    try {
      const cfg = await ConfigService.getApiV1ConfigNotifications();
      enabled = cfg.enabled;
      notifyNewReply = cfg.notify_new_reply;
      suppressSubagents = cfg.suppress_subagents ?? true;
      mergeWindow = String(cfg.merge_window_seconds ?? 60);
      agents = (cfg.agents ?? []).join(", ");
      projects = (cfg.projects ?? []).join(", ");
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  function csv(value: string): string[] | undefined {
    const items = value
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    return items.length > 0 ? items : undefined;
  }

  async function save() {
    if (saving) return;
    saving = true;
    error = null;
    success = null;
    const seconds = Number.parseInt(mergeWindow, 10);
    const body: NotificationsConfigBody = {
      enabled,
      notify_new_reply: notifyNewReply,
      merge_window_seconds:
        Number.isFinite(seconds) && seconds > 0 ? seconds : undefined,
      agents: csv(agents),
      projects: csv(projects),
      suppress_subagents: suppressSubagents,
    };
    try {
      await ConfigService.postApiV1ConfigNotifications(body);
      success = m.settings_notifications_saved();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }
</script>

<div class="notifications-settings">
  <div class="setting-row">
    <span class="setting-label">{m.settings_notifications_enable()}</span>
    <Toggle
      checked={enabled}
      ariaLabel={m.settings_notifications_enable()}
      onchange={(v) => (enabled = v)}
    />
  </div>

  <p class="hint">{m.settings_notifications_turn_end_hint()}</p>

  <div class="setting-row">
    <div class="setting-text">
      <span class="setting-label">{m.settings_notifications_new_reply()}</span>
      <span class="hint">{m.settings_notifications_new_reply_hint()}</span>
    </div>
    <Toggle
      checked={notifyNewReply}
      ariaLabel={m.settings_notifications_new_reply()}
      onchange={(v) => (notifyNewReply = v)}
    />
  </div>

  {#if notifyNewReply}
    <div class="setting-row column">
      <label class="setting-label" for="notifications-merge">
        {m.settings_notifications_merge_window()}
        <span class="hint">{m.settings_notifications_merge_window_hint()}</span>
      </label>
      <TextInput
        id="notifications-merge"
        class="setting-input"
        size="md"
        type="text"
        bind:value={mergeWindow}
      />
    </div>
  {/if}

  <div class="setting-row">
    <span class="setting-label">{m.settings_notifications_suppress_subagents()}</span>
    <Toggle
      checked={suppressSubagents}
      ariaLabel={m.settings_notifications_suppress_subagents()}
      onchange={(v) => (suppressSubagents = v)}
    />
  </div>

  <div class="setting-row column">
    <label class="setting-label" for="notifications-agents">
      {m.settings_notifications_agents()}
      <span class="hint">{m.settings_notifications_agents_hint()}</span>
    </label>
    <TextInput
      id="notifications-agents"
      class="setting-input"
      size="md"
      block
      type="text"
      placeholder="claude, codex"
      bind:value={agents}
    />
  </div>

  <div class="setting-row column">
    <label class="setting-label" for="notifications-projects">
      {m.settings_notifications_projects()}
      <span class="hint">{m.settings_notifications_projects_hint()}</span>
    </label>
    <TextInput
      id="notifications-projects"
      class="setting-input"
      size="md"
      block
      type="text"
      bind:value={projects}
    />
  </div>

  {#if error}
    <div class="notifications-error">{error}</div>
  {/if}
  {#if success}
    <div class="notifications-success">{success}</div>
  {/if}

  <div class="save-row">
    <button class="save-btn" disabled={saving} onclick={save}>
      {saving ? m.settings_notifications_saving() : m.settings_notifications_save()}
    </button>
  </div>
</div>

<style>
  .notifications-settings {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  .setting-text {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .hint {
    margin: 0;
  }
  .notifications-error {
    color: var(--color-danger, #b3261e);
  }
  .notifications-success {
    color: var(--color-success, #1b7f4d);
  }
  .save-row {
    display: flex;
    justify-content: flex-end;
  }
</style>
