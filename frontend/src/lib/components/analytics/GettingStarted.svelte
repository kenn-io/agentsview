<script lang="ts">
  import { Card, IconButton } from "@kenn-io/kit-ui";
  import { CheckIcon, XIcon } from "../../icons.js";
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
  <Card level="default" padding="none" class="getting-started" title={m.getting_started_title()}>
    {#snippet actions()}
      <IconButton
        size="sm"
        ariaLabel={m.getting_started_dismiss()}
        onclick={() => gettingStarted.dismiss()}
      >
        <XIcon size="14" strokeWidth="2" aria-hidden="true" />
      </IconButton>
    {/snippet}
    <ul class="items" aria-label={m.getting_started_title()}>
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
              <CheckIcon size="16" strokeWidth="2" aria-hidden="true" />
            {:else}
              <span class="circle" aria-hidden="true"></span>
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
    display: grid;
    grid-template-columns: repeat(4, max-content);
    gap: var(--space-4) var(--space-8);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  @media (max-width: 1100px) {
    .items {
      grid-template-columns: repeat(2, max-content);
    }
  }

  @media (max-width: 560px) {
    .items {
      grid-template-columns: max-content;
    }
  }

  .item {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .mark {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    width: 16px;
    height: 16px;
    color: var(--text-muted);
  }

  .circle {
    width: 14px;
    height: 14px;
    border: 1.5px solid var(--text-muted);
    border-radius: 3px;
  }

  .label {
    min-width: 0;
    padding: 0;
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
