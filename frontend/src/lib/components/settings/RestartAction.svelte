<!-- A settings notice with the daemon restart beside it. Settings notices
     share one restart, so the busy and failed states live in the store.
     Where the server cannot restart itself, the notice says to
     restart the way AgentsView was started, unless `manual` is off because
     the notice needs no restart. -->
<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { settings } from "../../stores/settings.svelte.js";

  let { notice, manual = true }: { notice: string; manual?: boolean } = $props();
</script>

<div class="restart-action">
  {#if !settings.restartSupported}
    <p class="restart-text" role="status" aria-live="polite">
      {notice}
      {#if manual}{m.settings_restart_manual()}{/if}
    </p>
  {:else if settings.restartFailed && !settings.restartError}
    <p class="restart-text" role="status" aria-live="polite">
      {notice}
      {#if settings.restartCanRetry}
        {m.settings_restart_consequence()}
      {:else}
        {m.settings_restart_reconnect_failed()}
      {/if}
    </p>
    {#if settings.restartCanRetry}
      <Button disabled={settings.saving || settings.restarting} onclick={() => settings.restartDaemon()}>
        {m.settings_restart_now()}
      </Button>
    {/if}
    <Button disabled={settings.restarting} onclick={() => settings.checkConnection()}>
      {settings.restarting ? m.settings_restart_restarting() : m.settings_restart_check_connection()}
    </Button>
  {:else}
    <p class="restart-text" role="status" aria-live="polite">
      {notice}
      {m.settings_restart_consequence()}
      {#if settings.requireAuth}{m.settings_restart_sign_in()}{/if}
    </p>
    <Button
      disabled={settings.saving || settings.restarting}
      onclick={() => settings.restartDaemon()}
    >
      {settings.restarting ? m.settings_restart_restarting() : m.settings_restart_now()}
    </Button>
  {/if}
  {#if settings.restartFailed}
    <div class="restart-action restart-recovery" role="alert">
      {#if settings.restartError}
        <p class="restart-text restart-error">{m.settings_restart_refused()}</p>
        <p class="restart-text">
          {m.settings_restart_reason({ error: settings.restartError.replace(/[.\s]+$/, "") })}
        </p>
      {/if}
      <ol class="restart-text restart-steps">
        <li>{m.settings_restart_address()}</li>
        <li>{m.settings_restart_if_stopped()}</li>
      </ol>
    </div>
  {/if}
</div>

<style>
  /* Notice, button, and recovery stack in the control column so they share
     its left edge, like the hint above. */
  .restart-action {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
    width: 100%;
    min-width: 0;
    margin: 0;
    padding: 0;
  }

  .restart-text {
    margin: 0;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.4;
  }

  /* The recovery steps are a separate block from the notice and button. */
  .restart-recovery {
    margin-top: var(--space-2);
  }

  /* Numbered so the two commands read as steps, not alternatives. */
  .restart-steps {
    padding-left: 1.6em;
    list-style: decimal;
  }

  .restart-steps li + li {
    margin-top: var(--space-2);
  }

  /* The localized headline carries the error color; the server's own
     reason follows it as muted detail. */
  .restart-error {
    color: var(--accent-red, #ef4444);
  }
</style>
