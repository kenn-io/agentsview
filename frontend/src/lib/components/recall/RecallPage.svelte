<script lang="ts">
  import {
    SegmentedControl,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import GeneratedInsightsPanel from "./GeneratedInsightsPanel.svelte";
  import RecallCorpusPanel from "./RecallCorpusPanel.svelte";

  type RecallTab = "corpus" | "generated";

  const backendKnown = $derived(
    sync.serverVersion !== null ||
      (settings.loaded && settings.error === null),
  );
  const corpusAvailable = $derived(
    backendKnown && !sync.readOnly && !settings.readOnly,
  );
  const tabOptions = $derived.by((): SegmentedControlOption[] => [
    ...(corpusAvailable
      ? [{ value: "corpus", label: m.recall_page_tab_corpus() }]
      : []),
    {
      value: "generated",
      label: m.recall_page_tab_generated(),
    },
  ]);
  const selectedTab = $derived.by((): RecallTab => {
    if (Object.hasOwn(router.params, "insight")) return "generated";
    if (router.params.tab === "generated") return "generated";
    return corpusAvailable ? "corpus" : "generated";
  });

  function selectTab(value: string) {
    const next = { ...router.params };
    delete next.insight;
    if (value === "generated") {
      next.tab = "generated";
    } else {
      delete next.tab;
    }
    router.replaceParams(next);
  }
</script>

<div class="recall-page">
  <header class="recall-workspace-header">
    <div>
      <h1>{m.recall_page_title()}</h1>
      <p>{m.recall_page_workspace_subtitle()}</p>
    </div>
    {#if tabOptions.length > 1}
      <SegmentedControl
        options={tabOptions}
        value={selectedTab}
        ariaLabel={m.recall_page_tabs_label()}
        onchange={selectTab}
      />
    {/if}
  </header>

  {#if !backendKnown}
    <p class="workspace-loading">{m.recall_page_loading()}</p>
  {:else if selectedTab === "corpus"}
    <RecallCorpusPanel />
  {:else}
    <GeneratedInsightsPanel />
  {/if}
</div>

<style>
  .recall-page {
    min-height: 100%;
  }

  .recall-workspace-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    padding: 1.25rem 1.5rem 0;
  }

  .recall-workspace-header h1 {
    margin: 0;
    color: var(--text-primary);
    font-size: 1.25rem;
  }

  .recall-workspace-header p {
    margin: 0.25rem 0 0;
    color: var(--text-secondary);
    font-size: 0.82rem;
  }

  .workspace-loading {
    margin: 0;
    padding: 1.5rem;
    color: var(--text-secondary);
    font-size: 0.82rem;
  }

  @media (max-width: 760px) {
    .recall-workspace-header {
      align-items: stretch;
      flex-direction: column;
      padding: 1rem 1rem 0;
    }
  }
</style>
