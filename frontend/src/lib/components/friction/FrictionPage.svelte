<script lang="ts">
  import { onDestroy, onMount, untrack } from "svelte";
  import { Button, Card, EmptyState, IconButton, Typeahead } from "@kenn-io/kit-ui";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { ChevronLeftIcon, ChevronRightIcon } from "../../icons.js";
  import { friction } from "../../stores/friction.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import RefreshControl from "../shared/RefreshControl.svelte";
  import FrictionHeadline from "./FrictionHeadline.svelte";
  import FrictionMarkdownPanel from "./FrictionMarkdownPanel.svelte";
  import FrictionReviewSummaryPanel from "./FrictionReviewSummaryPanel.svelte";
  import FrictionSignalSection from "./FrictionSignalSection.svelte";
  import FrictionSummaryStrip from "./FrictionSummaryStrip.svelte";
  import { groupSignals } from "./frictionView.js";

  const DOCS_URL = "https://agentsview.io/docs/configuration/#friction-log";

  const digest = $derived(friction.digest);
  const sections = $derived(groupSignals(digest?.signals ?? []));
  const flagged = $derived(
    (digest?.signals?.length ?? 0) > 0 || (digest?.p0_alerts?.length ?? 0) > 0,
  );
  const busy = $derived(friction.loading.dates || friction.loading.digest);
  // friction_available means the server has a friction runner, which can
  // build even where the session archive is read-only, as on pg serve.
  const frictionAvailable = $derived(sync.serverVersion?.friction_available === true);
  // Builds also need a caller the server accepts: localhost, or any caller
  // when the server requires auth. A proxied page without auth cannot build.
  const buildAllowed = $derived(sync.serverVersion?.friction_build_allowed === true);
  const dateOptions = $derived(
    friction.dates.map((item) => ({ name: item.date, label: item.date })),
  );

  const routeDate = $derived(router.params["date"] ?? null);
  // Read before any effect runs: the store may still hold a date from an
  // earlier visit, and the URL must win.
  const initialDate = untrack(() => routeDate);
  // The page settles after its first successful list load, whoever started
  // it: this page, Retry, or Build now.
  const listLoadsAtMount = untrack(() => friction.listLoads);
  const settled = $derived(friction.listLoads > listLoadsAtMount);

  // The URL owns the selected date, so history and shared links work while
  // the page stays mounted.
  function selectDate(date: string | null) {
    if (!date || date === friction.selectedDate) return;
    router.replaceParams({ date });
  }

  $effect(() => {
    const date = routeDate;
    untrack(() => {
      if (date && date !== friction.selectedDate && friction.dates.some((d) => d.date === date)) {
        void friction.selectDate(date);
      }
    });
  });

  // Once the first load settles, keep the URL on the digest the store chose,
  // such as the latest one.
  $effect(() => {
    const selected = friction.selectedDate;
    if (!settled) return;
    untrack(() => {
      if (selected && router.params["date"] !== selected) router.replaceParams({ date: selected });
    });
  });

  // Until a list load succeeds, the page keeps asking for what it opened
  // with: the URL's date, or the latest digest. A date left from an earlier
  // visit must not take over through a failed first load.
  function refresh() {
    void friction.load(settled ? friction.selectedDate : initialDate);
  }

  onMount(() => {
    void friction.load(initialDate);
  });

  onDestroy(() => {
    friction.cancelInFlightReads();
  });
</script>

<div class="friction-page">
  <header class="toolbar">
    <div class="date-nav" role="group" aria-label={m.friction_date_label()}>
      <IconButton
        size="sm"
        ariaLabel={m.friction_date_previous()}
        title={m.friction_date_previous()}
        disabled={friction.olderDate === null}
        onclick={() => selectDate(friction.olderDate)}
      >
        <ChevronLeftIcon size="14" aria-hidden="true" />
      </IconButton>
      <Typeahead
        options={dateOptions}
        value={friction.selectedDate ?? ""}
        fallbackLabel={friction.selectedDate ?? m.friction_date_label()}
        placeholder={m.friction_date_label()}
        title={m.friction_date_label()}
        emptyLabel={m.friction_date_no_match()}
        onselect={(value) => selectDate(value)}
      />
      <IconButton
        size="sm"
        ariaLabel={m.friction_date_next()}
        title={m.friction_date_next()}
        disabled={friction.newerDate === null}
        onclick={() => selectDate(friction.newerDate)}
      >
        <ChevronRightIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>

    {#if buildAllowed}
      <Button
        size="sm"
        label={m.friction_build_now()}
        title={m.friction_build_title()}
        disabled={friction.building}
        onclick={() => void friction.buildNow()}
      />
    {/if}

    <RefreshControl
      lastUpdatedAt={friction.lastUpdatedAt}
      queryDurationMs={friction.lastQueryDurationMs}
      {busy}
      onRefresh={refresh}
      label={m.friction_page_refresh()}
      title={m.friction_page_refresh()}
    />
  </header>

  <main class="content" aria-busy={busy}>
    {#if friction.lastBuildWritten !== null}
      <div class="build-status" role="status">
        {friction.lastBuildWritten > 0
          ? m.friction_build_done({ count: friction.lastBuildWritten })
          : m.friction_build_nothing()}
      </div>
    {/if}
    {#if friction.errors.build}
      <div class="build-status error" role="alert">
        {m.friction_build_failed({ error: friction.errors.build })}
      </div>
    {/if}

    {#if friction.errors.dates}
      <Card level="default" padding="none" class="state-panel">
        <div class="state-panel-alert" role="alert">
          <strong>{m.friction_load_error()}</strong>
          <span>{friction.errors.dates}</span>
          <Button size="sm" label={m.friction_retry()} onclick={refresh} />
        </div>
      </Card>
    {:else if !friction.loading.dates && friction.dates.length === 0}
      {#if frictionAvailable}
        <EmptyState title={m.friction_empty_title()}>
          <p class="empty-hint">
            {buildAllowed ? m.friction_empty_hint() : m.friction_empty_hint_remote()}
          </p>
        </EmptyState>
      {:else}
        <EmptyState title={m.friction_disabled_title()}>
          <p class="empty-hint">{m.friction_disabled_hint()}</p>
        </EmptyState>
      {/if}
    {:else if friction.errors.digest}
      <Card level="default" padding="none" class="state-panel">
        <div class="state-panel-alert" role="alert">
          <strong>{m.friction_load_error()}</strong>
          <span>{friction.errors.digest}</span>
          <Button
            size="sm"
            label={m.friction_retry()}
            onclick={() => friction.selectedDate && void friction.selectDate(friction.selectedDate)}
          />
        </div>
      </Card>
    {:else if digest}
      <section class="digest-meta">
        <h1>{m.friction_digest_heading({ date: digest.date })}</h1>
        <p>
          {m.friction_scanned({ count: digest.sessions_scanned })}
          · {m.friction_digest_built({
            when: formatDateTime(digest.built_at, { dateStyle: "medium", timeStyle: "short" }),
            timezone: digest.timezone,
          })}
          ·
          <a href={DOCS_URL} target="_blank" rel="noopener noreferrer" class="docs-link">
            {m.friction_page_help_docs()}
          </a>
        </p>
      </section>

      {#if flagged}
        <FrictionSummaryStrip {sections} />

        <FrictionHeadline
          date={digest.date}
          p0Alerts={digest.p0_alerts}
          signals={digest.signals ?? []}
          patterns={digest.patterns ?? []}
          sessions={digest.sessions}
        />

        <FrictionReviewSummaryPanel date={digest.date} revision={digest.revision} />

        {#each sections as section (section.kind)}
          {#if section.signals.length > 0}
            <FrictionSignalSection {section} sessions={digest.sessions} />
          {/if}
        {/each}
      {:else}
        <EmptyState title={m.friction_clean_title()}>
          <p class="empty-hint">
            {digest.sessions_scanned > 0
              ? m.friction_clean_hint({ count: digest.sessions_scanned })
              : m.friction_clean_no_sessions()}
          </p>
        </EmptyState>
      {/if}

      <FrictionMarkdownPanel date={digest.date} />
    {/if}
  </main>
</div>

<style>
  .friction-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
    background: var(--bg-primary);
  }
  .toolbar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px;
    padding: 8px 16px;
    border-bottom: 1px solid var(--border-muted);
  }
  .toolbar :global(.kit-refresh-control) {
    margin-left: auto;
  }
  .date-nav {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .date-nav :global(.kit-typeahead) {
    width: 170px;
    min-width: 170px;
    flex: 0 0 170px;
  }
  .content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 18px;
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
    max-width: 1100px;
  }
  .docs-link {
    color: var(--accent-blue);
  }
  .digest-meta h1 {
    margin: 0;
    font-size: 18px;
  }
  .digest-meta p {
    margin: 4px 0 0;
    color: var(--text-muted);
    font-size: 12px;
  }
  .build-status {
    font-size: 12px;
    color: var(--text-secondary);
  }
  .build-status.error {
    color: var(--accent-red);
  }
  .state-panel-alert {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 12px;
    font-size: 12px;
    align-items: flex-start;
  }
  .empty-hint {
    max-width: 60ch;
  }
</style>
