<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { callGenerated, isNotFoundError } from "../../api/runtime.js";
  import {
    getApiV1SettingsDuplicateGroups,
    postApiV1SettingsDuplicateGroupsRebuild,
  } from "../../api/generated/settings/settings.js";
  import type { DbDuplicateGroupInfo } from "../../api/generated/models/dbDuplicateGroupInfo.js";
  import { router } from "../../stores/router.svelte.js";

  interface Props {
    readOnly?: boolean;
  }

  let { readOnly = false }: Props = $props();

  let groups = $state<DbDuplicateGroupInfo[]>([]);
  let loading = $state(false);
  let rebuilding = $state(false);
  let loadError = $state("");
  let loaded = $state(false);
  let unavailable = $state(false);

  const totalMembers = $derived(
    groups.reduce((sum, g) => sum + g.members.length, 0),
  );

  function formatStarted(ts: string | null | undefined): string {
    if (!ts) return "";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return ts;
    return d.toLocaleString();
  }

  async function load() {
    // Listing is a local-only settings endpoint: it answers 501 in remote
    // or read-only mode, so skip the doomed request and show the
    // unavailable state instead of surfacing a raw API error.
    if (readOnly) {
      unavailable = true;
      return;
    }
    unavailable = false;
    loading = true;
    loadError = "";
    try {
      groups = await callGenerated(() => getApiV1SettingsDuplicateGroups());
      loaded = true;
    } catch (err) {
      if (!isNotFoundError(err)) {
        loadError = err instanceof Error ? err.message : String(err);
      }
    } finally {
      loading = false;
    }
  }

  async function rebuild() {
    if (rebuilding || readOnly) return;
    rebuilding = true;
    try {
      await callGenerated(() =>
        postApiV1SettingsDuplicateGroupsRebuild({}),
      );
      await load();
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      rebuilding = false;
    }
  }

  function openSession(id: string) {
    const href = router.buildSessionHref(id);
    window.open(href, "_blank", "noopener");
  }

  onMountIdle(load);

  function onMountIdle(fn: () => void) {
    // Kick off the first load lazily so the card never blocks the
    // rules view's initial paint.
    setTimeout(fn, 0);
  }
</script>

<section class="duplicate-review">
  <header class="review-header">
    <h3>{m.duplicate_review_title()}</h3>
    <p class="review-description">{m.duplicate_review_description()}</p>
  </header>

  {#if readOnly}
    <div class="review-readonly" role="note">
      {m.duplicate_review_local_only()}
    </div>
  {:else}
    <div class="review-actions">
      <button class="rebuild-btn" onclick={rebuild} disabled={rebuilding}>
        {rebuilding
          ? m.duplicate_review_rebuilding()
          : m.duplicate_review_recheck()}
      </button>
    </div>
  {/if}

  {#if loadError}
    <div class="review-error" role="alert">{loadError}</div>
  {/if}

  {#if unavailable}
    <div class="review-status">{m.duplicate_review_local_only()}</div>
  {:else if loading && !loaded}
    <div class="review-status">{m.data_loading()}</div>
  {:else if groups.length === 0}
    <div class="review-status">{m.duplicate_review_empty()}</div>
  {:else}
    <div class="review-groups">
      {#each groups as group (group.group_key)}
        <div class="group-card">
          <div class="group-heading">
            {m.duplicate_review_group_heading({
              count: group.members.length,
            })}
          </div>
          <table class="group-table">
            <thead>
              <tr>
                <th>{m.duplicate_review_role_canonical()}</th>
                <th>{m.duplicate_review_agent()}</th>
                <th>{m.duplicate_review_started()}</th>
                <th>{m.duplicate_review_messages()}</th>
              </tr>
            </thead>
            <tbody>
              {#each group.members as member (member.session_id)}
                <tr>
                  <td>
                    <button
                      class="member-link"
                      onclick={() => openSession(member.session_id)}
                    >
                      {member.session_id}
                    </button>
                  </td>
                  <td>{member.agent}</td>
                  <td>{formatStarted(member.started_at)}</td>
                  <td>{member.message_count}</td>
                  <td>
                    <span
                      class="role-tag"
                      class:canonical={member.role === "canonical"}
                    >
                      {member.role === "canonical"
                        ? m.duplicate_review_role_canonical()
                        : m.duplicate_review_role_duplicate()}
                    </span>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .duplicate-review {
    border: 1px solid var(--border-muted);
    border-radius: 6px;
    padding: 12px 14px;
    margin-top: 16px;
  }

  .review-header h3 {
    margin: 0 0 4px;
    font-size: 13px;
  }

  .review-description {
    margin: 0;
    font-size: 11px;
    color: var(--text-muted);
  }

  .review-actions {
    margin: 10px 0;
  }

  .rebuild-btn {
    font-size: 11px;
    padding: 3px 10px;
  }

  .review-readonly,
  .review-status,
  .review-error {
    font-size: 11px;
    color: var(--text-muted);
    margin: 8px 0;
  }

  .review-error {
    color: var(--accent-red, #d9534f);
  }

  .review-groups {
    display: flex;
    flex-direction: column;
    gap: var(--space-3, 12px);
    margin-top: 8px;
  }

  .group-card {
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    padding: 8px 10px;
  }

  .group-heading {
    font-size: 11px;
    font-weight: 600;
    margin-bottom: 6px;
  }

  .group-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 11px;
  }

  .group-table th,
  .group-table td {
    text-align: left;
    padding: 2px 8px 2px 0;
  }

  .member-link {
    font: inherit;
    background: none;
    border: none;
    padding: 0;
    color: var(--accent-blue);
    cursor: pointer;
    text-align: left;
  }

  .member-link:hover {
    text-decoration: underline;
  }

  .role-tag {
    font-size: 10px;
    padding: 1px 6px;
    border-radius: 8px;
    border: 1px solid var(--border-muted);
    color: var(--text-muted);
  }

  .role-tag.canonical {
    color: var(--accent-green, #2e7d32);
  }
</style>
