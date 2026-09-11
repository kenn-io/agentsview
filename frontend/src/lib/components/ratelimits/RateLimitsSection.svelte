<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { Card } from "@kenn-io/kit-ui";
  import { rateLimits } from "../../stores/ratelimits.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { usage } from "../../stores/usage.svelte.js";
  import { localDateRangeToUTCBounds } from "../../utils/rateLimitFormat.js";
  import { m } from "../../i18n/index.js";
  import RateLimitCard from "./RateLimitCard.svelte";

  interface Props {
    /** The Usage page's active date range (YYYY-MM-DD), reused for each
     * card's history chart when no narrower time range is brushed. */
    from: string;
    to: string;
  }

  let { from, to }: Props = $props();

  // The Usage page's own brushed time-range selection narrows the active
  // window without changing `from`/`to`; prefer it when set so a card's
  // history chart matches whatever range Usage is actually showing.
  const effectiveFrom = $derived(usage.selectedTimeRange?.from ?? from);
  const effectiveTo = $derived(usage.selectedTimeRange?.to ?? to);
  const bounds = $derived(localDateRangeToUTCBounds(effectiveFrom, effectiveTo));
  const since = $derived(bounds.since);
  const until = $derived(bounds.until);

  // Excluding an agent on the Usage page hides only that vendor's group,
  // not the whole section, so another vendor's cards (once another
  // vendor writes rows) would keep showing.
  const visibleVendorGroups = $derived(
    rateLimits.groupedByVendor.filter((group) => !usage.isAgentExcluded(group.vendor)),
  );

  // planType is deliberately not part of this key: Codex reports it as a
  // label that can flip between "pro" and empty for the same window from
  // one observation to the next (see docs/agents/storage.md), not a
  // stable identity component, so keying on it would render a plan-type
  // flip as a new card instead of an update to the existing one.
  function snapshotKey(snapshot: (typeof rateLimits.current)[number]): string {
    return [
      snapshot.vendor,
      snapshot.accountId ?? "",
      snapshot.machine,
      snapshot.limitId,
      snapshot.windowKind,
    ].join(" ");
  }

  function vendorLabel(vendor: string): string {
    // Every composed UI label -- including vendor names -- goes through
    // the Paraglide message dictionaries so all locales stay in sync; the
    // vendor identifier itself ("codex") is untranslated data used only
    // to select the message key.
    switch (vendor) {
      case "codex":
        return m.rate_limits_vendor_codex();
      default:
        return vendor;
    }
  }

  onMount(() => {
    rateLimits.fetchCurrent();
  });

  $effect(() => {
    // Re-fetch the current snapshots when the shared machine or agent
    // filter changes; history re-fetches itself per card via its own
    // $effect.
    void sessions.filters.machine;
    void sessions.filters.agent;
    untrack(() => rateLimits.fetchCurrent());
  });
</script>

{#if rateLimits.hasData && visibleVendorGroups.length > 0}
  <Card level="default" padding="none" class="chart-panel wide">
    <div class="section-header">
      <h2>{m.rate_limits_section_title()}</h2>
    </div>
    {#each visibleVendorGroups as vendorGroup (vendorGroup.vendor)}
      <div class="vendor-group">
        {#if visibleVendorGroups.length > 1}
          <h3 class="vendor-title">{vendorLabel(vendorGroup.vendor)}</h3>
        {/if}
        {#each vendorGroup.accounts as accountGroup (accountGroup.key)}
          <div class="account-group">
            {#if accountGroup.accountLabel}
              <h4 class="account-title">{accountGroup.accountLabel}</h4>
            {/if}
            <div class="rate-limits-grid">
              {#each accountGroup.windows as snapshot (snapshotKey(snapshot))}
                <RateLimitCard window={snapshot} {since} {until} />
              {/each}
            </div>
          </div>
        {/each}
      </div>
    {/each}
  </Card>
{/if}

<style>
  .section-header {
    display: flex;
    align-items: center;
    margin-bottom: 10px;
  }

  .section-header h2 {
    font-size: 13px;
    font-weight: 600;
    color: var(--text-primary);
    margin: 0;
  }

  .vendor-group + .vendor-group {
    margin-top: 16px;
  }

  .vendor-title {
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-secondary);
    margin: 0 0 8px;
  }

  .account-group + .account-group {
    margin-top: 10px;
  }

  .account-title {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    margin: 0 0 6px;
  }

  .rate-limits-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
    gap: 12px;
  }
</style>
