<script module lang="ts">
  // Builds whose completed last_result has already driven one automatic
  // search retry. Module-level so it survives the remounts the command palette
  // performs each time the underlying search 501s again: a retired generation
  // whose daemon still reports the old last_result would otherwise loop
  // search + status forever. One auto-retry per observed build, then the panel
  // shows setup controls instead. Keyed on build_id plus started_at because
  // build_id is daemon-process-local and restarts from 1 after a daemon
  // restart.
  const resolvedBuildIds = new Set<string>();

  function resolvedBuildKey(status: {
    build_id?: number;
    started_at?: string;
  }): string {
    return `${status.build_id ?? 0}:${status.started_at ?? ""}`;
  }

  // Testing-only: clear the module-level retry ledger between cases so remount
  // behavior can be exercised in isolation.
  export function __resetResolvedBuildIds(): void {
    resolvedBuildIds.clear();
  }
</script>

<script lang="ts">
  import { onMount } from "svelte";
  import { Button, CopyButton, Spinner } from "@kenn-io/kit-ui";
  import { getLocale, m } from "../../i18n/index.js";
  import { EmbeddingsService } from "../../api/generated/index.js";
  import { ApiError, callGenerated, isAbortError } from "../../api/runtime.js";
  import { copyToClipboard } from "../../utils/clipboard.js";

  interface Props {
    // Called when a build finishes successfully so the caller can re-run
    // the search that failed.
    onResolved: () => void;
    // The search's own 501 message. Most carry the generic "enable [vector]
    // ... run 'agentsview embeddings build'" remediation the panel replaces,
    // but some name a specific recoverable state (stale index after an
    // embedding-config change, vectors.db schema mismatch) that the status
    // probe cannot distinguish — those are shown verbatim so their
    // remediation is not hidden.
    searchDetail?: string | null;
  }

  let { onResolved, searchDetail = null }: Props = $props();

  // db.ErrSemanticUnavailable's base message; search failures wrapping a
  // more specific cause differ from it.
  const GENERIC_SEARCH_UNAVAILABLE =
    "semantic search not available: enable [vector] in config.toml and run " +
    "'agentsview embeddings build'";

  const specificSearchDetail = $derived(
    searchDetail !== null && searchDetail !== GENERIC_SEARCH_UNAVAILABLE
      ? searchDetail
      : null,
  );

  // The daemon returns this exact 501 message when [vector] is disabled in
  // config.toml (no embeddings manager wired). Any other 501 reason (e.g.
  // vector serving disabled because another process held vectors.write.lock)
  // is already actionable and is shown verbatim instead of the config
  // walkthrough.
  const GENERIC_UNAVAILABLE = "embeddings manager not available";

  const CONFIG_SNIPPET = `[vector]
enabled = true

[vector.embeddings]
model = "nomic-embed-text"
dimension = 768

[vector.embeddings.servers.local]
endpoint = "http://localhost:11434/v1"`;

  const BUILD_COMMAND = "agentsview embeddings build";
  const BUILD_POLL_MS = 2000;

  type Phase =
    | "probing"
    | "unconfigured"
    | "disabled"
    | "ready"
    | "building"
    | "failed";

  let phase = $state<Phase>("probing");
  // Server-provided reason shown verbatim in the "disabled" and "failed"
  // phases.
  let detail = $state<string | null>(null);
  // What the "failed" phase's Retry button should do. A failure that came from
  // a build (persisted last_error, or a failed build POST) can only be cleared
  // by starting a new build; re-probing would just re-read the same persisted
  // last_error. Transport/API failures from the status calls retry by probing.
  let retryAction = $state<"probe" | "build">("probe");
  let buildDone = $state(0);
  let buildTotal = $state(0);
  let scanning = $state(true);

  let timer: ReturnType<typeof setTimeout> | null = null;
  let disposed = false;
  let copiedConfig = $state(false);
  let copiedCommand = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | null = null;

  const percentFormat = $derived(
    new Intl.NumberFormat(getLocale(), {
      style: "percent",
      maximumFractionDigits: 0,
    }),
  );

  onMount(() => {
    void probe();
    return () => {
      disposed = true;
      if (timer !== null) clearTimeout(timer);
      if (copyTimer !== null) clearTimeout(copyTimer);
    };
  });

  async function probe(): Promise<void> {
    try {
      const status = await callGenerated(() =>
        EmbeddingsService.getApiV1EmbeddingsStatus(),
      );
      if (disposed) return;
      if (status.running) {
        applyProgress(status.phase, status.done, status.total);
        phase = "building";
        schedulePoll();
      } else if (status.last_error) {
        detail = status.last_error;
        retryAction = "build";
        phase = "failed";
      } else if (status.last_result) {
        const buildKey = resolvedBuildKey(status);
        if (resolvedBuildIds.has(buildKey)) {
          // Already auto-retried this build once and the search still 501s, so
          // stop looping and offer the setup controls instead.
          phase = "ready";
        } else {
          resolvedBuildIds.add(buildKey);
          onResolved();
        }
      } else {
        phase = "ready";
      }
    } catch (e) {
      if (disposed || isAbortError(e)) return;
      if (e instanceof ApiError && e.status === 501) {
        if (specificSearchDetail) {
          detail = specificSearchDetail;
          phase = "disabled";
        } else if (e.message && e.message !== GENERIC_UNAVAILABLE) {
          detail = e.message;
          phase = "disabled";
        } else {
          phase = "unconfigured";
        }
        return;
      }
      detail = e instanceof Error ? e.message : null;
      retryAction = "probe";
      phase = "failed";
    }
  }

  async function startBuild(): Promise<void> {
    phase = "building";
    scanning = true;
    buildDone = 0;
    buildTotal = 0;
    try {
      await callGenerated(() =>
        EmbeddingsService.postApiV1EmbeddingsBuild({ requestBody: {} }),
      );
    } catch (e) {
      if (disposed || isAbortError(e)) return;
      // 409 means a build is already running (started by the CLI or the
      // scheduler); watching it is exactly what the user wants.
      if (!(e instanceof ApiError && e.status === 409)) {
        detail = e instanceof Error ? e.message : null;
        retryAction = "build";
        phase = "failed";
        return;
      }
    }
    if (disposed) return;
    schedulePoll();
  }

  function schedulePoll(): void {
    if (timer !== null) clearTimeout(timer);
    timer = setTimeout(() => void poll(), BUILD_POLL_MS);
  }

  async function poll(): Promise<void> {
    try {
      const status = await callGenerated(() =>
        EmbeddingsService.getApiV1EmbeddingsStatus(),
      );
      if (disposed) return;
      if (status.running) {
        applyProgress(status.phase, status.done, status.total);
        schedulePoll();
        return;
      }
      if (status.last_error) {
        detail = status.last_error;
        retryAction = "build";
        phase = "failed";
        return;
      }
      resolvedBuildIds.add(resolvedBuildKey(status));
      onResolved();
    } catch (e) {
      if (disposed || isAbortError(e)) return;
      detail = e instanceof Error ? e.message : null;
      retryAction = "probe";
      phase = "failed";
    }
  }

  function applyProgress(
    buildPhase: string | undefined,
    done: number,
    total: number,
  ): void {
    scanning = buildPhase === "scanning" || total <= 0;
    buildDone = done;
    buildTotal = total;
  }

  async function copy(text: string, which: "config" | "command"): Promise<void> {
    const ok = await copyToClipboard(text);
    if (!ok || disposed) return;
    if (copyTimer !== null) clearTimeout(copyTimer);
    copiedConfig = which === "config";
    copiedCommand = which === "command";
    copyTimer = setTimeout(() => {
      copiedConfig = false;
      copiedCommand = false;
    }, 1500);
  }
</script>

<div class="semantic-setup" role="alert">
  {#if phase === "probing"}
    <div class="setup-row centered">
      <Spinner size={13} />
    </div>
  {:else if phase === "unconfigured"}
    <strong>{m.command_palette_semantic_setup_title()}</strong>
    <span class="setup-text">{m.command_palette_semantic_setup_config_step()}</span>
    <div class="setup-snippet">
      <CopyButton
        class="setup-copy"
        copied={copiedConfig}
        ariaLabel={m.command_palette_semantic_copy_config()}
        copiedAriaLabel={m.command_palette_semantic_copied()}
        title={m.command_palette_semantic_copy_config()}
        copiedTitle={m.command_palette_semantic_copied()}
        onclick={() => void copy(CONFIG_SNIPPET, "config")}
      />
      <pre>{CONFIG_SNIPPET}</pre>
    </div>
    <span class="setup-text">{m.command_palette_semantic_setup_build_step()}</span>
    <div class="setup-snippet">
      <CopyButton
        class="setup-copy"
        copied={copiedCommand}
        ariaLabel={m.command_palette_semantic_copy_command()}
        copiedAriaLabel={m.command_palette_semantic_copied()}
        title={m.command_palette_semantic_copy_command()}
        copiedTitle={m.command_palette_semantic_copied()}
        onclick={() => void copy(BUILD_COMMAND, "command")}
      />
      <pre>{BUILD_COMMAND}</pre>
    </div>
  {:else if phase === "disabled"}
    <strong>{m.command_palette_semantic_disabled_title()}</strong>
    <span class="setup-text">{detail}</span>
  {:else if phase === "ready"}
    <strong>{m.command_palette_semantic_ready_title()}</strong>
    {#if specificSearchDetail}
      <span class="setup-text mono">{specificSearchDetail}</span>
    {:else}
      <span class="setup-text">{m.command_palette_semantic_ready_intro()}</span>
    {/if}
    <div class="setup-action">
      <Button
        size="sm"
        tone="info"
        surface="soft"
        label={m.command_palette_semantic_build_button()}
        onclick={() => void startBuild()}
      />
    </div>
    <span class="setup-hint">{m.command_palette_semantic_build_hint()}</span>
  {:else if phase === "building"}
    <div class="setup-row centered">
      <Spinner size={13} />
      <strong>{m.command_palette_semantic_building_title()}</strong>
    </div>
    <span class="setup-text" aria-live="polite">
      {#if scanning}
        {m.command_palette_semantic_building_scanning()}
      {:else}
        {m.command_palette_semantic_building_progress({
          percentLabel: percentFormat.format(
            buildTotal > 0 ? Math.min(1, buildDone / buildTotal) : 0,
          ),
        })}
      {/if}
    </span>
    <span class="setup-hint">{m.command_palette_semantic_build_hint()}</span>
  {:else}
    <strong>{m.command_palette_semantic_failed_title()}</strong>
    {#if detail}
      <span class="setup-text">{detail}</span>
    {/if}
    <div class="setup-action">
      <Button
        size="sm"
        tone="info"
        surface="soft"
        label={m.shared_retry()}
        onclick={() => {
          detail = null;
          if (retryAction === "build") {
            void startBuild();
          } else {
            phase = "probing";
            void probe();
          }
        }}
      />
    </div>
  {/if}
</div>

<style>
  .semantic-setup {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 16px;
    text-align: left;
    color: var(--text-muted);
    font-size: 13px;
  }

  .semantic-setup strong {
    color: var(--text-primary);
  }

  .setup-text {
    font-size: 12px;
  }

  .setup-text.mono {
    font-family: var(--font-mono);
    font-size: 11px;
  }

  .setup-hint {
    font-size: 11px;
    color: var(--text-muted);
  }

  .setup-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .setup-row.centered {
    justify-content: center;
  }

  .setup-snippet {
    position: relative;
    background: var(--code-bg);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .setup-snippet pre {
    margin: 0;
    padding: 8px 12px;
    font-family: var(--font-mono);
    font-size: 11px;
    line-height: 1.5;
    color: var(--code-text);
    overflow-x: auto;
  }

  :global(.setup-copy.kit-copy-btn) {
    position: absolute;
    top: 4px;
    right: 4px;
    z-index: 1;
  }

  .setup-snippet:hover :global(.setup-copy.kit-copy-btn) {
    opacity: 1;
  }

  .setup-action {
    display: flex;
    margin-top: 4px;
  }
</style>
