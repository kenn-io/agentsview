<script lang="ts">
  import { Card, IconButton } from "@kenn-io/kit-ui";
  import { CircleCheckIcon, CircleIcon, XIcon } from "../../icons.js";
  import { gettingStarted, items, type Item } from "../../stores/gettingStarted.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { m } from "../../i18n/index.js";

  const uid = $props.id();

  function openFirstSession() {
    const first = sessions.sessions[0];
    if (first) router.navigateToSession(first.id);
  }

  const steps: Record<Item, { label: () => string; run: () => void }> = {
    session: { label: m.getting_started_open_session, run: openFirstSession },
    search: { label: m.getting_started_search, run: () => (ui.activeModal = "commandPalette") },
    usage: {
      label: m.getting_started_usage,
      run: () => router.navigate("usage", { view: "tokens" }),
    },
    insights: {
      label: m.getting_started_insights,
      run: () => router.navigate("recall", { tab: "generated" }),
    },
  };
</script>

{#if gettingStarted.visible}
  <Card level="default" padding="sm" class="getting-started" title={m.getting_started_title()}>
    {#snippet actions()}
      <IconButton
        size="sm"
        ariaLabel={m.getting_started_dismiss()}
        onclick={() => gettingStarted.dismiss()}
      >
        <XIcon size="14" strokeWidth="2" aria-hidden="true" />
      </IconButton>
    {/snippet}
    <ul class="items">
      {#each items as key (key)}
        {@const done = gettingStarted.done.has(key)}
        <li class="item" class:done>
          <span
            class="mark"
            role="checkbox"
            aria-checked={done}
            aria-readonly="true"
            aria-labelledby="{uid}-{key}"
          >
            {#if done}
              <CircleCheckIcon size="16" strokeWidth="2" aria-hidden="true" />
            {:else}
              <CircleIcon size="16" strokeWidth="1.5" aria-hidden="true" />
            {/if}
          </span>
          <button
            type="button"
            class="label"
            id="{uid}-{key}"
            disabled={key === "session" && sessions.sessions.length === 0}
            onclick={steps[key].run}
          >
            {steps[key].label()}
          </button>
        </li>
      {/each}
    </ul>
  </Card>
{/if}

<style>
  .items {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-4) var(--space-8);
    list-style: none;
  }

  .item {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .mark {
    display: inline-flex;
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .label {
    min-width: 0;
    font-size: 12px;
    text-align: left;
  }

  .label:hover:not(:disabled) {
    color: var(--accent-blue);
    text-decoration: underline;
  }

  .done .label {
    color: var(--text-muted);
  }
</style>
