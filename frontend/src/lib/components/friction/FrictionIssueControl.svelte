<script lang="ts">
  import { Button, TextInput } from "@kenn-io/kit-ui";
  import type { FrictionIssueItem } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { friction } from "../../stores/friction.svelte.js";
  import { fileableKind } from "./frictionView.js";

  interface Props {
    fingerprint: string;
    kind: string;
    issue?: FrictionIssueItem | null;
    // The server accepts filing from this page and Kata is ready.
    canFile: boolean;
  }

  let { fingerprint, kind, issue = null, canFile }: Props = $props();

  let linking = $state(false);
  let ref = $state("");
  const busy = $derived(friction.issueBusy[fingerprint] === true);
  const error = $derived(friction.issueErrors[fingerprint]);

  async function link() {
    const value = ref.trim();
    if (!value) return;
    await friction.linkIssue(fingerprint, value);
    if (!friction.issueErrors[fingerprint]) {
      linking = false;
      ref = "";
    }
  }

  function cancel() {
    linking = false;
    ref = "";
  }
</script>

<span class="issue-control">
  {#if issue}
    {#if issue.web_url}
      <a
        class="issue-link"
        href={issue.web_url}
        target="_blank"
        rel="noopener noreferrer"
        title={m.friction_issue_open({ issue: issue.qualified_id })}
      >
        {issue.qualified_id}
      </a>
    {:else}
      <span class="issue-link">{issue.qualified_id}</span>
    {/if}
    {#if canFile}
      <Button
        size="sm"
        surface="outline"
        label={m.friction_issue_unlink()}
        title={m.friction_issue_unlink_title()}
        {busy}
        onclick={() => void friction.unlinkIssue(fingerprint)}
      />
    {/if}
  {:else if canFile && fileableKind(kind)}
    {#if linking}
      <TextInput
        size="sm"
        bind:value={ref}
        placeholder={m.friction_issue_link_placeholder()}
        ariaLabel={m.friction_issue_link_label()}
        autofocus
        onkeydown={(event) => {
          if (event.key === "Enter") void link();
          if (event.key === "Escape") cancel();
        }}
      />
      <Button size="sm" label={m.friction_issue_link_submit()} {busy} onclick={() => void link()} />
      <Button size="sm" surface="outline" label={m.friction_issue_cancel()} onclick={cancel} />
    {:else}
      <Button
        size="sm"
        label={m.friction_issue_file()}
        title={m.friction_issue_file_title()}
        {busy}
        onclick={() => void friction.fileIssue(fingerprint)}
      />
      <Button
        size="sm"
        surface="outline"
        label={m.friction_issue_link()}
        disabled={busy}
        onclick={() => (linking = true)}
      />
    {/if}
  {/if}
  {#if error}
    <span class="issue-error" role="alert">{error}</span>
  {/if}
</span>

<style>
  .issue-control {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }
  .issue-link {
    font-family: var(--font-mono);
    font-size: 12px;
    color: var(--accent-blue);
  }
  .issue-error {
    flex-basis: 100%;
    font-size: 12px;
    color: var(--accent-red);
  }
</style>
