<script lang="ts">
  import { Toggle } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { notificationsAvailable } from "../../notifications.js";

  const available = notificationsAvailable();
  let saving = $state(false);
  let checked = $state(false);
  $effect(() => {
    checked = settings.notifications.enabled;
  });

  async function toggle(enabled: boolean) {
    saving = true;
    try {
      await settings.save({ notifications: { enabled } });
    } finally {
      checked = settings.notifications.enabled;
      saving = false;
    }
  }
</script>

<Toggle
  bind:checked
  disabled={!available || saving || settings.saving || settings.readOnly}
  ariaLabel={m.settings_notifications_enable()}
  onchange={toggle}
>
  {m.settings_notifications_enable()}
</Toggle>
<p class="muted">{m.settings_notifications_turn_end_hint()}</p>
{#if !available}
  <p class="msg" role="status">{m.settings_notifications_unavailable()}</p>
{/if}

<style>
  .muted {
    font-size: 12px;
    color: var(--text-muted);
    margin: 0;
  }

  .msg {
    font-size: 11px;
    margin: 0;
  }
</style>
