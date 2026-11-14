<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import type { DbSignalCalibration as SignalCalibration } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { formatCount } from "../../utils/format.js";
  import {
    formatQualityDate,
    type QualityPatternSeverity,
    type QualityPatternView,
  } from "./qualityPatterns.js";

  // One quality pattern: its severity, how many sessions it affects, the
  // signals that drive it, its trend, and examples. Picking a signal asks
  // the page to show the sessions behind it.
  interface Props {
    pattern: QualityPatternView;
    calibration: Record<string, SignalCalibration> | undefined;
    selectedSignalId: string | null;
    onselectsignal: (signal: string) => void;
  }

  let { pattern, calibration, selectedSignalId, onselectsignal }: Props = $props();

  function severityLabel(severity: QualityPatternSeverity): string {
    switch (severity) {
      case "critical":
        return m.insights_page_severity_critical();
      case "warning":
        return m.insights_page_severity_warning();
      case "watch":
        return m.insights_page_severity_watch();
      case "clear":
        return m.insights_page_severity_clear();
      case "unavailable":
        return m.insights_page_severity_unavailable();
    }
  }

  function affectedLabel(pattern: QualityPatternView): string {
    if (pattern.totalSessions === 0) return m.insights_page_no_computed_sessions();
    return m.insights_page_affected_sessions({ affected: formatCount(pattern.affectedSessions), total: formatCount(pattern.totalSessions) });
  }

  function pct(count: number, total: number): number {
    if (total <= 0) return 0;
    return Math.round((count / total) * 100);
  }

  function maxTrend(pattern: QualityPatternView): number {
    return Math.max(1, ...pattern.trend.map((p) => p.value));
  }

  function calibrationLabel(signal: string): string {
    if (signal.startsWith("outcome_")) {
      return m.insights_page_calibration_outcome_cohort();
    }
    const entry = calibration?.[signal] ?? null;
    if (!entry) {
      return m.insights_page_calibration_examples_only();
    }
    if (entry.affected_sessions === 0) {
      return m.insights_page_calibration_no_affected();
    }
    if (entry.incomplete_lift == null) {
      return m.insights_page_calibration_incomplete_rate({ rate: entry.affected_incomplete_rate });
    }
    return m.insights_page_calibration_incomplete_lift({ lift: entry.incomplete_lift.toFixed(1) });
  }
</script>

<Card
  level="default"
  padding="none"
  class={`pattern-card severity-${pattern.severity}`}
>
  <article
    class="pattern-card-content"
    aria-labelledby={`${pattern.id}-title`}
  >
  <div class="pattern-head">
    <div>
      <h3 id={`${pattern.id}-title`}>
        {pattern.title}
      </h3>
      <p>{pattern.summary}</p>
    </div>
    <span class="severity">
      {severityLabel(pattern.severity)}
    </span>
  </div>

  <div class="affected">
    <strong>{affectedLabel(pattern)}</strong>
    <span>
      {m.insights_page_pct_affected({ pct: pct(pattern.affectedSessions, pattern.totalSessions) })}
    </span>
  </div>

  <div class="driver-frame">
  <div class="driver-list">
    <div class="driver-head" aria-hidden="true">
      <span>{m.insights_page_column_signal()}</span>
      <span>{m.insights_page_column_count()}</span>
      <span>{m.insights_page_column_sessions()}</span>
      <span>{m.insights_page_column_outcome()}</span>
    </div>
    {#each pattern.drivers as driver}
      {@const sessionsLabel = m.insights_page_driver_sessions({
        count: driver.sessions,
        countLabel: formatCount(driver.sessions),
      })}
      {@const totalLabel = `${formatCount(driver.total)}${driver.unit ?? ""}`}
      {@const outcomeLabel = calibrationLabel(String(driver.id))}
      <button
        class="driver-row"
        class:active={selectedSignalId === driver.id}
        type="button"
        aria-label={`${driver.label}: ${totalLabel}, ${sessionsLabel}, ${outcomeLabel}`}
        onclick={() => onselectsignal(String(driver.id))}
      >
        <span>{driver.label}</span>
        <strong>{totalLabel}</strong>
        <em>{formatCount(driver.sessions)}</em>
        <small>{outcomeLabel}</small>
      </button>
    {/each}
  </div>
  </div>

  <div
    class="sparkline"
    aria-label={`${pattern.title}: ${pattern.trendLabel}`}
  >
    <span class="trend-caption">{pattern.trendLabel}</span>
    {#each pattern.trend.slice(-16) as point}
      <span
        title={`${formatQualityDate(point.date)}: ${point.value} ${point.label}`}
        style:height={`${Math.max(8, (point.value / maxTrend(pattern)) * 32)}px`}
      ></span>
    {/each}
  </div>
  <p class="severity-note">{pattern.severityDescription}</p>

  {#if pattern.examples.length > 0}
    <div class="examples">
      <span class="examples-label">{pattern.examplesLabel}</span>
      {#each pattern.examples as example}
        <div class="example-row">
          <span>{example.label}</span>
          <em>{example.detail}</em>
        </div>
      {/each}
    </div>
  {/if}
  </article>
</Card>

<style>
  .pattern-card-content {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: 12px;
    min-width: 0;
  }

  .pattern-head {
    display: flex;
    gap: 12px;
    justify-content: space-between;
    align-items: flex-start;
  }

  .pattern-head h3 {
    font-size: 14px;
    font-weight: var(--font-weight-semibold, 600);
    margin-bottom: 3px;
  }

  .pattern-head p {
    color: var(--text-muted);
    font-size: 12px;
    line-height: 1.4;
  }

  /* A leading status dot in the pill's color, so severity reads like
     every other status in the suite (dots), not as text alone. */
  .severity {
    flex-shrink: 0;
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    border-radius: 999px;
    padding: 2px 8px 2px 7px;
    font-size: 11px;
    font-weight: 600;
    border: 1px solid color-mix(in srgb, currentColor 30%, transparent);
  }

  .severity::before {
    content: "";
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: currentColor;
  }

  .affected {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-4);
    padding: 10px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .affected strong {
    color: var(--text-primary);
    font-size: 13px;
  }

  .affected span {
    color: var(--text-muted);
    font-size: 12px;
  }

  .driver-list {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto auto auto;
    column-gap: var(--space-4);
    row-gap: 6px;
  }

  .driver-head,
  .driver-row {
    grid-column: 1 / -1;
    display: grid;
    grid-template-columns: subgrid;
    align-items: baseline;
  }

  .driver-head {
    padding: 0 4px;
    color: var(--text-muted);
    font-size: 10.5px;
    font-weight: 600;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    white-space: nowrap;
  }

  .driver-head span:not(:first-child),
  .driver-row > :not(:first-child) {
    text-align: right;
  }

  /* Outcome reads as its own column, not a suffix of the session count. */
  .driver-head span:last-child,
  .driver-row > :last-child {
    padding-left: var(--space-6);
  }

  .driver-row {
    width: 100%;
    min-height: 24px;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    text-align: left;
  }

  .driver-row:hover,
  .driver-row.active {
    background: var(--bg-surface-hover);
  }

  .driver-row span {
    color: var(--text-secondary);
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .driver-row strong {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .driver-row em {
    color: var(--text-muted);
    font-style: normal;
    font-variant-numeric: tabular-nums;
  }

  .driver-row small {
    color: var(--text-muted);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  /* Narrow cards: four columns would squeeze the signal names to nothing,
     so the outcome moves to its own line under the name, and its column
     heading goes with it. */
  .driver-frame {
    container-type: inline-size;
  }

  @container (max-width: 400px) {
    .driver-list {
      grid-template-columns: minmax(0, 1fr) auto auto;
    }

    .driver-head span:last-child {
      display: none;
    }

    .driver-row > :last-child {
      grid-column: 1 / -1;
      padding-left: 0;
      text-align: left;
      white-space: normal;
    }
  }

  .sparkline {
    height: 42px;
    display: flex;
    align-items: end;
    gap: var(--space-1);
    padding: 6px 0 2px;
    border-top: 1px solid var(--border-muted);
    position: relative;
  }

  .sparkline span:not(.trend-caption) {
    width: 100%;
    min-width: 3px;
    max-width: 16px;
    background: color-mix(
      in srgb,
      var(--accent-blue) 48%,
      var(--border-muted)
    );
    border-radius: 2px 2px 0 0;
  }

  .trend-caption {
    align-self: start;
    width: auto;
    min-width: 118px;
    max-width: none;
    height: auto !important;
    margin-right: 8px;
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    background: transparent;
  }

  .severity-note {
    margin-top: -4px;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
  }

  .examples {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: auto;
  }

  .examples-label {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .example-row {
    display: grid;
    grid-template-columns: minmax(90px, 0.35fr) 1fr;
    gap: var(--space-4);
    font-size: 12px;
  }

  .example-row span {
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .example-row em {
    color: var(--text-muted);
    font-style: normal;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
