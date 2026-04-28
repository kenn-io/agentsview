<script lang="ts">
  import type { Session } from "../../api/types.js";

  interface Props {
    session: Session | null | undefined;
  }

  const { session }: Props = $props();

  const status = $derived(session?.termination_status ?? null);
</script>

{#if status === "tool_call_pending"}
  <div
    class="termination-banner"
    role="status"
    data-status="tool_call_pending"
  >
    This session ended with a tool call that never received a
    response. The agent process likely terminated before the tool
    finished.
  </div>
{:else if status === "truncated"}
  <div
    class="termination-banner"
    role="status"
    data-status="truncated"
  >
    The session file ends mid-write. The agent process likely
    terminated abruptly.
  </div>
{/if}

<style>
  .termination-banner {
    margin: 0 0 0.75rem;
    padding: 0.625rem 0.875rem;
    border-radius: var(--radius-md, 6px);
    font-size: 0.8125rem;
    line-height: 1.4;
    color: var(--accent-amber, #f59e0b);
    background: color-mix(
      in srgb,
      var(--accent-amber, #f59e0b) 8%,
      transparent
    );
    border: 1px solid
      color-mix(
        in srgb,
        var(--accent-amber, #f59e0b) 25%,
        transparent
      );
  }
</style>
