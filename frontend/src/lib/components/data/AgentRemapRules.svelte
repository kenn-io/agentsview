<script lang="ts">
  import {
    Button,
    Checkbox,
    Modal,
    TableHeaderCell,
    TextInput,
    Typeahead,
  } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import {
    SettingsService,
    type DbAgentRemapRule,
    type AgentRemapRuleRequest,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { agentTypeaheadOptions } from "../../utils/agents.js";

  interface Props {
    readOnly?: boolean;
    /** Called after each successful create/update/delete/apply mutation. */
    onMutated?: () => void;
  }

  let { readOnly = false, onMutated = undefined }: Props = $props();

  type Confirmation = { kind: "delete"; rule: DbAgentRemapRule };
  type PreviewState = {
    token: string;
    matched: number;
    samples: { id: string; current_agent: string; next_agent: string }[];
  };

  let rules: DbAgentRemapRule[] = $state([]);
  let loading = $state(true);
  let saving = $state(false);
  let applying = $state(false);
  let error = $state("");
  let applyMessage = $state("");
  let editingId: number | null = $state(null);
  let sourceAgent = $state("");
  let modelGlob = $state("");
  let idPrefix = $state("");
  let targetAgent = $state("");
  let enabled = $state(true);
  let preview: PreviewState | null = $state(null);
  let confirmation: Confirmation | null = $state(null);
  const rulesRead = new LatestRead();
  let disposed = false;

  const canSave = $derived(
    sourceAgent.trim() !== "" && targetAgent.trim() !== "",
  );

  // Agents known to the UI: everything in the archive (with session
  // counts) plus the built-in catalog. loadAgents() is dedup-guarded, so
  // calling it here is enough to populate sessions.agents on a direct
  // visit to the Data page.
  const agentOptions = $derived(agentTypeaheadOptions(sessions.agents));

  const prefixOptions = $derived(
    agentOptions.map((option) => ({
      ...option,
      // Session IDs are namespaced as "<agent>:..."; offer each known
      // agent's prefix with the colon included so selecting a row writes
      // the exact value the matcher expects. Display the raw slug (not
      // the capitalized agent label) so the row text matches the actual
      // prefix case, and zero the count — a prefix's popularity is not
      // the agent's session count.
      name: `${option.name}:`,
      label: `${option.name}:`,
      displayLabel: `${option.name}:`,
      count: 0,
    })),
  );

  $effect(() => {
    void loadRules();
    void sessions.loadAgents();
  });

  async function loadRules() {
    if (disposed) return;
    const signal = rulesRead.begin();
    loading = true;
    error = "";
    try {
      const res = await callGenerated(
        (options) => SettingsService.getApiV1SettingsAgentRemapRules(options),
        signal,
      );
      if (!rulesRead.isCurrent(signal)) return;
      rules = res ?? [];
    } catch (err) {
      if (isAbortError(err) || !rulesRead.isCurrent(signal)) return;
      error = err instanceof Error ? err.message : m.agent_remap_failed_load();
    } finally {
      if (rulesRead.finish(signal)) loading = false;
    }
  }

  onDestroy(() => {
    disposed = true;
    rulesRead.cancel();
  });

  function resetForm() {
    editingId = null;
    sourceAgent = "";
    modelGlob = "";
    idPrefix = "";
    targetAgent = "";
    enabled = true;
    confirmation = null;
  }

  function editRule(rule: DbAgentRemapRule) {
    editingId = rule.id;
    sourceAgent = rule.source_agent;
    modelGlob = rule.model_glob;
    idPrefix = rule.id_prefix;
    targetAgent = rule.target_agent;
    enabled = rule.enabled;
    applyMessage = "";
    error = "";
  }

  function ruleInput(): AgentRemapRuleRequest | null {
    if (!sourceAgent.trim() || !targetAgent.trim()) return null;
    return {
      source_agent: sourceAgent.trim(),
      model_glob: modelGlob.trim(),
      id_prefix: idPrefix.trim(),
      target_agent: targetAgent.trim(),
      enabled,
    };
  }

  async function saveRule() {
    const input = ruleInput();
    if (!input) return;
    saving = true;
    error = "";
    applyMessage = "";
    try {
      if (editingId == null) {
        await callGenerated(() =>
          SettingsService.postApiV1SettingsAgentRemapRules(input),
        );
      } else {
        await callGenerated(() =>
          SettingsService.putApiV1SettingsAgentRemapRulesById(
            { id: String(editingId) },
            input,
          ),
        );
      }
      onMutated?.();
      resetForm();
      await loadRules();
    } catch (err) {
      if (disposed) return;
      error = err instanceof Error ? err.message : m.agent_remap_failed_save();
    } finally {
      if (!disposed) saving = false;
    }
  }

  function requestDelete(rule: DbAgentRemapRule) {
    confirmation = { kind: "delete", rule };
  }

  async function removeRule(rule: DbAgentRemapRule) {
    saving = true;
    error = "";
    applyMessage = "";
    try {
      await callGenerated(() =>
        SettingsService.deleteApiV1SettingsAgentRemapRulesById({
          id: String(rule.id),
        }),
      );
      onMutated?.();
      if (editingId === rule.id) resetForm();
      await loadRules();
    } catch (err) {
      if (disposed) return;
      error =
        err instanceof Error ? err.message : m.agent_remap_failed_delete();
    } finally {
      if (!disposed) saving = false;
    }
  }

  async function confirmChange() {
    const pending = confirmation;
    confirmation = null;
    if (!pending) return;
    await removeRule(pending.rule);
  }

  async function loadPreview() {
    applying = true;
    error = "";
    applyMessage = "";
    try {
      const res = await callGenerated(() =>
        SettingsService.postApiV1SettingsAgentRemapRulesPreview({}),
      );
      preview = {
        token: res.token,
        matched: res.matched_sessions,
        samples: res.samples ?? [],
      };
      applyMessage = "";
    } catch (err) {
      if (disposed) return;
      preview = null;
      error = err instanceof Error ? err.message : m.agent_remap_failed_apply();
    } finally {
      if (!disposed) applying = false;
    }
  }

  async function applyRules() {
    if (!preview?.token) return;
    applying = true;
    error = "";
    try {
      const res = await callGenerated(() =>
        SettingsService.postApiV1SettingsAgentRemapRulesApply({
          token: preview?.token ?? "",
        }),
      );
      preview = null;
      onMutated?.();
      applyMessage = m.agent_remap_apply_result({
        matched: res.matched_sessions,
      });
      await loadRules();
    } catch (err) {
      if (disposed) return;
      // 409: the rule set or matching sessions changed since the preview.
      preview = null;
      error = err instanceof Error ? err.message : m.agent_remap_failed_apply();
    } finally {
      if (!disposed) applying = false;
    }
  }
</script>

{#snippet confirmationActions()}
  <Button
    label={m.agent_remap_cancel()}
    tone="neutral"
    surface="outline"
    disabled={saving}
    onclick={() => (confirmation = null)}
  />
  <Button
    label={m.agent_remap_delete_confirm_action()}
    tone="danger"
    surface="solid"
    disabled={saving}
    onclick={confirmChange}
  />
{/snippet}

<section class="rules-view">
  <div class="heading-row">
    <h3>{m.agent_remap_title()}</h3>
    <p class="description">{m.agent_remap_description()}</p>
  </div>

  {#if loading}
    <div class="muted">{m.agent_remap_loading()}</div>
  {:else if error && rules.length === 0}
    <div class="error-text">{error}</div>
  {:else}
    <div class="rules-list" aria-busy={loading}>
      {#if loading}
        <div class="muted">{m.agent_remap_loading()}</div>
      {:else if rules.length === 0}
        <div class="empty">{m.agent_remap_no_rules()}</div>
      {:else}
        <div class="table-scroll">
          <table class="table">
            <thead>
              <tr>
                <TableHeaderCell label={m.agent_remap_col_source()} />
                <TableHeaderCell label={m.agent_remap_col_pattern()} />
                <TableHeaderCell label={m.agent_remap_col_prefix()} />
                <TableHeaderCell label={m.agent_remap_col_target()} />
                <TableHeaderCell label={m.agent_remap_col_enabled()} />
                {#if !readOnly}
                  <TableHeaderCell label={m.agent_remap_col_actions()} />
                {/if}
              </tr>
            </thead>
            <tbody>
              {#each rules as rule (rule.id)}
                <tr class="rule-row" class:disabled={!rule.enabled}>
                  <td>{rule.source_agent}</td>
                  <td class="col-pattern" title={rule.model_glob}>
                    {rule.model_glob || "—"}
                  </td>
                  <td class="col-pattern" title={rule.id_prefix}>
                    {rule.id_prefix || "—"}
                  </td>
                  <td>{rule.target_agent}</td>
                  <td>{rule.enabled ? m.agent_remap_on() : m.agent_remap_off()}</td>
                  {#if !readOnly}
                    <td>
                      <div class="row-actions">
                        <Button
                          size="sm"
                          label={m.agent_remap_edit()}
                          onclick={() => editRule(rule)}
                        />
                        <Button
                          size="sm"
                          tone="danger"
                          label={m.agent_remap_delete()}
                          onclick={() => requestDelete(rule)}
                        />
                      </div>
                    </td>
                  {/if}
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>

    {#if readOnly}
      <p class="warning" role="note">{m.agent_remap_read_only()}</p>
    {:else}
      <div class="form-grid">
        <label class="field">
          <span>{m.agent_remap_source()}</span>
          <Typeahead
            options={agentOptions}
            value={sourceAgent}
            fallbackLabel={sourceAgent || m.agent_remap_select_agent()}
            placeholder={m.agent_remap_select_agent()}
            title={m.agent_remap_source()}
            emptyLabel={m.agent_remap_no_matching_agent()}
            onselect={(value) => {
              sourceAgent = value;
            }}
          />
          <div class="hint">{m.agent_remap_source_hint()}</div>
        </label>
        <label class="field">
          <span>{m.agent_remap_model_glob()}</span>
          <TextInput
            bind:value={modelGlob}
            block
            ariaLabel={m.agent_remap_model_glob()}
            placeholder="ossington-*|rosedale-*"
          />
          <div class="hint">{m.agent_remap_model_glob_hint()}</div>
        </label>
        <label class="field">
          <span>{m.agent_remap_id_prefix()}</span>
          <Typeahead
            options={prefixOptions}
            value={idPrefix}
            fallbackLabel={idPrefix || m.agent_remap_any_prefix()}
            placeholder={m.agent_remap_filter_prefix()}
            title={m.agent_remap_id_prefix()}
            emptyLabel={m.agent_remap_no_matching_prefix()}
            allowClear
            clearLabel={m.agent_remap_any_prefix()}
            allowCustom
            customLabel={m.agent_remap_use_custom_prefix({ query: "{query}" })}
            onselect={(value) => {
              idPrefix = value;
            }}
          />
          <div class="hint">{m.agent_remap_id_prefix_hint()}</div>
        </label>
        <label class="field">
          <span>{m.agent_remap_target()}</span>
          <Typeahead
            options={agentOptions}
            value={targetAgent}
            fallbackLabel={targetAgent || m.agent_remap_select_agent()}
            placeholder={m.agent_remap_select_agent()}
            title={m.agent_remap_target()}
            emptyLabel={m.agent_remap_no_matching_agent()}
            onselect={(value) => {
              targetAgent = value;
            }}
          />
        </label>
        <div class="field checkbox-field">
          <Checkbox bind:checked={enabled} label={m.agent_remap_enabled()} />
        </div>
      </div>
    {/if}

    {#if preview}
      <div class="preview-panel">
        <div class="preview-summary">
          {m.agent_remap_preview_matched({ matched: preview.matched })}
        </div>
        {#if preview.samples.length > 0}
          <ul class="preview-samples">
            {#each preview.samples as sample (sample.id)}
              <li>
                <span class="mono">{sample.id}</span>
                <span class="arrow">&rarr;</span>
                {sample.current_agent}
                <span class="arrow">&rarr;</span>
                <strong>{sample.next_agent}</strong>
              </li>
            {/each}
          </ul>
        {/if}
        <div class="button-row">
          <Button
            label={m.agent_remap_preview_cancel()}
            disabled={applying}
            onclick={() => (preview = null)}
          />
          <Button
            label={applying
              ? m.agent_remap_applying()
              : m.agent_remap_apply_confirm()}
            tone="info"
            surface="solid"
            disabled={applying}
            onclick={applyRules}
          />
        </div>
      </div>
    {/if}

    {#if error}
      <div class="error-text">{error}</div>
    {/if}
    {#if applyMessage}
      <div class="success-text">{applyMessage}</div>
    {/if}

    {#if !readOnly}
      <div class="button-row">
        <Button
          label={saving
            ? m.agent_remap_saving()
            : editingId == null
              ? m.agent_remap_add_rule()
              : m.agent_remap_save_rule()}
          tone="info"
          surface="solid"
          disabled={!canSave || saving}
          onclick={saveRule}
        />
        {#if editingId != null}
          <Button
            label={m.agent_remap_cancel()}
            disabled={saving}
            onclick={resetForm}
          />
        {/if}
        <Button
          label={applying ? m.agent_remap_applying() : m.agent_remap_preview()}
          disabled={applying || loading || rules.length === 0}
          onclick={loadPreview}
        />
      </div>
    {/if}
  {/if}
</section>

{#if confirmation}
  <Modal
    title={m.agent_remap_delete_confirm_title()}
    closeLabel={m.agent_remap_confirmation_close()}
    tone="danger"
    width="440px"
    onclose={() => (confirmation = null)}
    footer={confirmationActions}
  >
    <p class="confirmation-copy">{m.agent_remap_change_warning()}</p>
  </Modal>
{/if}

<style>
  .rules-view {
    display: flex;
    flex-direction: column;
    gap: 12px;
    min-width: 0;
  }

  .heading-row {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  h3 {
    margin: 0;
    font-size: 13px;
  }

  .description {
    margin: 0;
    color: var(--text-secondary);
    font-size: 12px;
  }

  .button-row,
  .row-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .field > span {
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
  }

  .table-scroll {
    max-height: 420px;
    overflow-y: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }

  .table {
    width: 100%;
    border-collapse: collapse;
    font-size: 11px;
  }

  .table :global(thead th) {
    position: sticky;
    top: 0;
    z-index: 1;
  }

  tbody td {
    padding: 5px 8px;
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .rule-row:last-child td {
    border-bottom: none;
  }

  .rule-row.disabled {
    opacity: 0.65;
  }

  .col-pattern {
    max-width: 240px;
    overflow: hidden;
    text-overflow: ellipsis;
    font-family: var(--font-mono, monospace);
  }

  .hint,
  .muted,
  .empty {
    color: var(--text-muted);
    font-size: 11px;
  }

  .form-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }

  .checkbox-field {
    justify-content: flex-end;
  }

  .preview-panel {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 12px;
  }

  .preview-samples {
    margin: 0;
    padding: 0 0 0 16px;
    color: var(--text-secondary);
  }

  .preview-samples .mono {
    font-family: var(--font-mono, monospace);
  }

  .arrow {
    color: var(--text-muted);
    padding: 0 4px;
  }

  .warning {
    margin: 0;
    color: var(--accent-orange);
    font-size: 12px;
  }

  .error-text,
  .success-text {
    font-size: 12px;
  }

  .error-text {
    color: var(--accent-red);
  }

  .success-text {
    color: var(--accent-green);
  }

  .confirmation-copy {
    margin: 0;
    color: var(--text-secondary);
    font-size: 13px;
    line-height: 1.5;
  }

  @media (max-width: 760px) {
    .form-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
