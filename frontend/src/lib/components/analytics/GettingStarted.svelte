<script lang="ts">
  import { Card, IconButton } from "@kenn-io/kit-ui";
  import { CheckIcon, XIcon } from "../../icons.js";
  import { gettingStarted, type Item } from "../../stores/gettingStarted.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { m } from "../../i18n/index.js";

  const uid = $props.id();

  function openFirstSession() {
    const first = sessions.sessions[0];
    if (first) router.navigateToSession(first.id);
  }

  const items: {
    key: Item;
    label: () => string;
    run: () => void;
  }[] = [
    {
      key: "session",
      label: m.getting_started_open_session,
      run: openFirstSession,
    },
    {
      key: "search",
      label: m.getting_started_search,
      run: () => (ui.activeModal = "commandPalette"),
    },
    {
      key: "usage",
      label: m.getting_started_usage,
      run: () => router.navigate("usage", { view: "tokens" }),
    },
    {
      key: "insights",
      label: m.getting_started_insights,
      run: () => router.navigate("recall", { tab: "generated" }),
    },
  ];

  function isDisabled(key: Item): boolean {
    return key === "session" && sessions.sessions.length === 0;
  }
</script>

{#if gettingStarted.visible}
  <Card level="default" padding="none" class="getting-started">
    <div class="header">
      <h3 class="title" id="{uid}-title">{m.getting_started_title()}</h3>
      <IconButton
        size="sm"
        ariaLabel={m.getting_started_dismiss()}
        onclick={() => gettingStarted.dismiss()}
      >
        <XIcon size="14" strokeWidth="2" aria-hidden="true" />
      </IconButton>
    </div>
    <ul class="items" aria-labelledby="{uid}-title">
      {#each items as item (item.key)}
        {@const done = gettingStarted.done.has(item.key)}
        <li class="item" class:done>
          <span
            class="mark"
            role="checkbox"
            aria-checked={done}
            aria-readonly="true"
            aria-labelledby="{uid}-{item.key}"
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
            id="{uid}-{item.key}"
            disabled={isDisabled(item.key)}
            onclick={item.run}
          >
            {item.label()}
          </button>
        </li>
      {/each}
    </ul>
  </Card>
{/if}

<style>
  :global(.getting-started) {
    flex-shrink: 0;
  }

  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 8px;
  }

  .title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .items {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: var(--space-4);
    margin: 0;
    padding: 0;
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
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    width: 16px;
    height: 16px;
    color: var(--text-muted);
  }

  .circle {
    width: 12px;
    height: 12px;
    border: 1.5px solid var(--text-muted);
    border-radius: 50%;
  }

  .label {
    min-width: 0;
    padding: 0;
    font-size: 12px;
    color: var(--text-primary);
    text-align: left;
    cursor: pointer;
  }

  .label:hover:not(:disabled) {
    color: var(--accent-blue);
    text-decoration: underline;
  }

  .label:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  .label:disabled {
    color: var(--text-muted);
    cursor: default;
  }

  .done .label {
    color: var(--text-muted);
  }

</style>
