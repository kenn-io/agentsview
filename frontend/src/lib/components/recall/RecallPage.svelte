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
  import LibraryPageHeader from "../shared/LibraryPageHeader.svelte";

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
  <div class="recall-workspace-header">
    <LibraryPageHeader
      title={m.recall_page_title()}
      description={m.recall_page_workspace_subtitle()}
    >
      {#snippet actions()}
        {#if tabOptions.length > 1}
          <SegmentedControl
            options={tabOptions}
            value={selectedTab}
            ariaLabel={m.recall_page_tabs_label()}
            onchange={selectTab}
          />
        {/if}
      {/snippet}
    </LibraryPageHeader>
  </div>

  {#if !backendKnown}
    <p class="workspace-loading">{m.recall_page_loading()}</p>
  {:else if selectedTab === "corpus"}
    <RecallCorpusPanel />
  {:else}
    <GeneratedInsightsPanel />
  {/if}
</div>

<style>
  /* Shared library page frame: a 1040px content column. */
  .recall-page {
    min-height: 100%;
    max-width: 1088px;
    margin: 0 auto;
  }

  .recall-workspace-header {
    padding: 28px 24px 0;
  }

  .workspace-loading {
    margin: 0;
    padding: 0 24px;
    color: var(--text-secondary);
    font-size: 13px;
  }

  @media (max-width: 760px) {
    .recall-workspace-header {
      padding: 1rem 1rem 0;
    }
  }
</style>
