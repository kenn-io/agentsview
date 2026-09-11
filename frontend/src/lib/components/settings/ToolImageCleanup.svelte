<script lang="ts">
  import { Button, Modal, TextInput } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import { getLocale, m } from "../../i18n/index.js";
  import { DataService } from "../../api/generated/index";
  import type { DbStripImagesReport } from "../../api/generated/index";
  import { ApiError, callGenerated, isAbortError } from "../../api/runtime.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { settings } from "../../stores/settings.svelte.js";

  let phase = $state<
    "idle" | "previewing" | "previewed" | "confirming" | "applying" | "applied" | "failed"
  >("idle");
  let projectFilter = $state("");
  let beforeFilter = $state("");
  let frozenFilter = $state<{ project: string; before: string } | null>(null);
  let preview = $state<DbStripImagesReport | null>(null);
  let applied = $state<DbStripImagesReport | null>(null);
  let errorText = $state("");
  let mayHavePartiallyApplied = $state(false);
  let unavailableReason = $state<string | null>(null);
  const previewRead = new LatestRead();
  let disposed = false;

  onDestroy(() => {
    disposed = true;
    previewRead.cancel();
  });

  function formatBytes(value: number): string {
    const thresholds = [1 << 30, 1 << 20, 1 << 10, 1] as const;
    const units = ["gigabyte", "megabyte", "kilobyte", "byte"] as const;
    for (let i = 0; i < thresholds.length; i++) {
      const threshold = thresholds[i];
      if (threshold !== undefined && value >= threshold) {
        return new Intl.NumberFormat(getLocale(), {
          style: "unit",
          unit: units[i]!,
          unitDisplay: "short",
          maximumFractionDigits: 1,
        }).format(value / threshold);
      }
    }
    return new Intl.NumberFormat(getLocale(), {
      style: "unit",
      unit: "byte",
      unitDisplay: "short",
      maximumFractionDigits: 0,
    }).format(0);
  }

  function formatCount(value: number): string {
    return new Intl.NumberFormat(getLocale()).format(value);
  }

  function onFilterInput() {
    // A rewrite in flight owns the frozen filter it was confirmed with, so an
    // edit must not reopen the panel underneath it.
    if (phase === "applying") return;
    // Drop any preview still in flight, or its late response would become the
    // accepted report for a selection the user has since changed.
    previewRead.cancel();
    preview = null;
    errorText = "";
    mayHavePartiallyApplied = false;
    if (phase !== "idle") phase = "idle";
  }

  async function runPreview() {
    if (phase === "previewing" || phase === "applying" || phase === "confirming") return;
    phase = "previewing";
    unavailableReason = null;
    errorText = "";
    mayHavePartiallyApplied = false;
    const signal = previewRead.begin();
    try {
      const report = await callGenerated(
        (options) =>
          DataService.postApiV1DataStripImagesPreview(
            { project: projectFilter, before: beforeFilter },
            options,
          ),
        signal,
      );
      if (disposed || !previewRead.isCurrent(signal)) return;
      preview = report;
      phase = "previewed";
    } catch (e) {
      if (disposed || isAbortError(e) || !previewRead.isCurrent(signal)) return;
      if (e instanceof ApiError && e.status === 403) {
        // The archive is local; the request reached the server through a proxy
        // or forwarded port, so name that rather than the remote-backend case.
        unavailableReason = m.settings_image_cleanup_proxy_denied();
        phase = "idle";
      } else if (e instanceof ApiError && e.status === 501) {
        unavailableReason = m.settings_image_cleanup_unavailable();
        phase = "idle";
      } else {
        errorText =
          e instanceof ApiError && e.message
            ? e.message
            : m.settings_image_cleanup_preview_failed();
        phase = "failed";
      }
    } finally {
      previewRead.finish(signal);
    }
  }

  function openConfirm() {
    frozenFilter = { project: projectFilter, before: beforeFilter };
    phase = "confirming";
  }

  function cancelConfirm() {
    phase = "previewed";
  }

  async function confirmApply() {
    if (!frozenFilter) return;
    phase = "applying";
    errorText = "";
    mayHavePartiallyApplied = false;
    try {
      const report = await callGenerated((options) =>
        DataService.postApiV1DataStripImages(
          {
            project: frozenFilter!.project,
            before: frozenFilter!.before,
            confirmed: true,
          },
          options,
        ),
      );
      if (disposed) return;
      applied = report;
      phase = "applied";
    } catch (e) {
      if (disposed || isAbortError(e)) return;
      if (e instanceof ApiError && e.status === 409) {
        errorText = m.settings_image_cleanup_busy();
        phase = "previewed";
      } else if (e instanceof ApiError && e.status === 503) {
        // A closed writer during shutdown or a maintenance pass is transient
        // and carries its own Retry-After message; nothing was rewritten.
        errorText = e.message || m.settings_image_cleanup_busy();
        phase = "previewed";
      } else {
        mayHavePartiallyApplied = true;
        errorText = e instanceof ApiError && e.message ? e.message : "";
        preview = null;
        phase = "failed";
      }
    }
  }

  const previewBusy = $derived(
    phase === "previewing" || phase === "applying" || phase === "confirming",
  );
  const canApply = $derived(
    phase === "previewed" &&
      preview !== null &&
      preview.payloads > 0 &&
      !settings.readOnly,
  );
</script>

<div class="tool-image-cleanup">
  <p class="intro">{m.settings_image_cleanup_intro()}</p>

  <div class="filter-row">
    <label class="filter-label" for="tic-project">
      {m.settings_image_cleanup_project_label()}
    </label>
    <TextInput
      id="tic-project"
      placeholder={m.settings_image_cleanup_project_placeholder()}
      bind:value={projectFilter}
      disabled={phase === "applying"}
      oninput={onFilterInput}
    />
    <label class="filter-label" for="tic-before">
      {m.settings_image_cleanup_before_label()}
    </label>
    <TextInput
      id="tic-before"
      placeholder={m.settings_image_cleanup_before_placeholder()}
      bind:value={beforeFilter}
      disabled={phase === "applying"}
      oninput={onFilterInput}
    />
  </div>

  <div class="action-row">
    <Button onclick={runPreview} disabled={previewBusy}>
      {phase === "previewing"
        ? m.settings_image_cleanup_previewing()
        : m.settings_image_cleanup_preview()}
    </Button>
  </div>

  {#if unavailableReason !== null}
    <p class="msg muted">{unavailableReason}</p>
  {/if}

  {#if settings.readOnly && phase !== "idle"}
    <p class="msg muted">{m.settings_image_cleanup_read_only()}</p>
  {/if}

  {#if errorText || mayHavePartiallyApplied}
    <p class="msg error" role="alert">
      {#if errorText}{errorText}{/if}
      {#if mayHavePartiallyApplied}
        {errorText ? " " : ""}{m.settings_image_cleanup_apply_failed()}
      {/if}
    </p>
  {/if}

  {#if preview !== null && (phase === "previewed" || phase === "confirming" || phase === "applying")}
    {#if preview.payloads === 0}
      <p class="msg muted">{m.settings_image_cleanup_empty()}</p>
    {:else}
      <div class="status-grid">
        <span class="row-label">{m.settings_image_cleanup_column_sessions()}</span>
        <span class="row-value">
          {m.settings_image_cleanup_sessions({
            count: preview.sessions,
            countLabel: formatCount(preview.sessions),
          })}
        </span>
        <span class="row-label">{m.settings_image_cleanup_column_payloads()}</span>
        <span class="row-value">
          {m.settings_image_cleanup_payloads({
            count: Number(preview.payloads),
            countLabel: formatCount(Number(preview.payloads)),
          })}
        </span>
        <span class="row-label">{m.settings_image_cleanup_column_stored()}</span>
        <span class="row-value">{formatBytes(Number(preview.stored_bytes))}</span>
        <span class="row-label">{m.settings_image_cleanup_column_decoded()}</span>
        <span class="row-value">{formatBytes(Number(preview.decoded_bytes))}</span>
      </div>

      {#if preview.projects && preview.projects.length > 0}
        <div class="by-project">
          <p class="subsection-title">{m.settings_image_cleanup_by_project()}</p>
          <table class="project-table">
            <thead>
              <tr>
                <th>{m.settings_image_cleanup_column_project()}</th>
                <th>{m.settings_image_cleanup_column_sessions()}</th>
                <th>{m.settings_image_cleanup_column_payloads()}</th>
                <th>{m.settings_image_cleanup_column_stored()}</th>
                <th>{m.settings_image_cleanup_column_decoded()}</th>
              </tr>
            </thead>
            <tbody>
              {#each preview.projects as proj (proj.project)}
                <tr>
                  <td class="project-name">{proj.project || m.shared_none()}</td>
                  <td>{formatCount(proj.sessions)}</td>
                  <td>{formatCount(Number(proj.payloads))}</td>
                  <td>{formatBytes(Number(proj.stored_bytes))}</td>
                  <td>{formatBytes(Number(proj.decoded_bytes))}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}

      <p class="compact-note muted">{m.settings_image_cleanup_compact_note()}</p>

      <div class="apply-row">
        <Button
          tone="danger"
          surface="solid"
          disabled={!canApply || phase === "applying"}
          onclick={openConfirm}
        >
          {phase === "applying"
            ? m.settings_image_cleanup_applying()
            : m.settings_image_cleanup_apply()}
        </Button>
      </div>
    {/if}
  {/if}

  {#if phase === "applied" && applied !== null}
    <div class="applied-summary">
      <p class="applied-text">
        {m.settings_image_cleanup_applied()}
        {" "}
        {m.settings_image_cleanup_changed({
          count: applied.changed,
          countLabel: formatCount(applied.changed),
        })}
      </p>
    </div>
  {/if}
</div>

{#if phase === "confirming"}
  {#snippet confirmActions()}
    <Button label={m.settings_image_cleanup_cancel()} onclick={cancelConfirm} />
    <Button
      label={m.settings_image_cleanup_confirm_action()}
      tone="danger"
      surface="solid"
      onclick={confirmApply}
    />
  {/snippet}
  <Modal
    title={m.settings_image_cleanup_confirm_title()}
    closeLabel={m.settings_image_cleanup_confirm_close()}
    tone="danger"
    width="440px"
    onclose={cancelConfirm}
    footer={confirmActions}
  >
    {#if frozenFilter}
      <dl class="confirm-filter">
        <dt>{m.settings_image_cleanup_project_label()}</dt>
        <dd>{frozenFilter.project || m.settings_image_cleanup_project_placeholder()}</dd>
        <dt>{m.settings_image_cleanup_before_label()}</dt>
        <dd>{frozenFilter.before || m.shared_none()}</dd>
      </dl>
    {/if}
    <p class="confirm-scope">{m.settings_image_cleanup_confirm_scope()}</p>
    <p class="confirm-body">{m.settings_image_cleanup_confirm_body()}</p>
    <p class="confirm-compact muted">{m.settings_image_cleanup_compact_note()}</p>
  </Modal>
{/if}

<style>
  .tool-image-cleanup {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }

  .intro {
    font-size: 12px;
    color: var(--text-secondary);
    margin: 0;
  }

  .filter-row {
    display: grid;
    grid-template-columns: auto 1fr;
    column-gap: 12px;
    row-gap: 8px;
    align-items: center;
  }

  .filter-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .action-row {
    display: flex;
    gap: 8px;
    align-items: center;
  }

  .msg {
    font-size: 11px;
    margin: 0;
  }

  .msg.muted {
    color: var(--text-muted);
  }

  .msg.error {
    color: var(--accent-red, #ef4444);
  }

  .status-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    column-gap: 16px;
    row-gap: 6px;
    align-items: start;
  }

  .row-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .row-value {
    font-size: 12px;
    color: var(--text-primary);
  }

  .by-project {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .subsection-title {
    margin: 0;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .project-table {
    font-size: 11px;
    border-collapse: collapse;
    width: 100%;
    min-width: 0;
  }

  .project-table th,
  .project-table td {
    padding: 4px 8px;
    text-align: left;
    border-bottom: 1px solid var(--border-muted);
    white-space: nowrap;
  }

  .project-table th {
    font-weight: 600;
    color: var(--text-secondary);
    background: var(--bg-inset);
  }

  .project-table td {
    color: var(--text-primary);
  }

  .project-name {
    font-family: var(--font-mono, monospace);
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .compact-note {
    font-size: 11px;
    color: var(--text-muted);
    margin: 0;
  }

  .apply-row {
    display: flex;
    gap: 8px;
  }

  .applied-summary {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .applied-text {
    font-size: 12px;
    color: var(--text-primary);
    margin: 0;
  }

  .confirm-scope {
    font-size: 12px;
    color: var(--text-secondary);
    margin: 0 0 8px;
  }

  .confirm-body {
    font-size: 12px;
    color: var(--text-primary);
    margin: 0;
  }

  .confirm-filter {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 2px 12px;
    font-size: 12px;
    margin: 0 0 8px;
  }

  .confirm-filter dt {
    color: var(--text-secondary);
  }

  .confirm-filter dd {
    color: var(--text-primary);
    margin: 0;
    overflow-wrap: anywhere;
  }

  .confirm-compact {
    font-size: 11px;
    color: var(--text-secondary);
    margin: 8px 0 0;
  }
</style>
