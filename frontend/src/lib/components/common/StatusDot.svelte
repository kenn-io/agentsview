<script lang="ts">
  import {
    getSessionStatus,
    type SessionStatusInput,
  } from "../../stores/sessions.svelte.js";

  interface Props {
    /** Anything with the recency + termination_status fields the
     * status calculation reads. Both the full Session and the
     * lighter TopSession (analytics top list) qualify. */
    session: SessionStatusInput;
    /** Optional full member list of this row's continuation/subagent
     * group. When provided, the status uses the freshest activity
     * across the group: a parent in tool_call_pending with a
     * subagent currently writing rolls up to working/green. The
     * parent's parser flag still wins for awaiting_user — a fork
     * running in parallel doesn't change that the agent has said
     * "your turn". */
    groupSessions?: SessionStatusInput[];
    size?: number;
  }

  let { session, groupSessions, size = 6 }: Props = $props();

  let status = $derived(getSessionStatus(session, groupSessions));

  let label = $derived.by(() => {
    switch (status) {
      case "working":
        return "Working — last write within the last minute";
      case "waiting":
        return "Waiting on user input";
      case "idle":
        return "Recently active, currently idle";
      case "stale":
        return "Flagged session, idle for 10–60 minutes";
      case "unclean":
        return "Terminated mid tool call (or file truncated)";
      case "quiet":
        return "";
    }
  });
</script>

{#if status === "waiting"}
  <!-- FontAwesome free comment-solid (CC BY 4.0). The path defines
       a speech bubble whose body (the ellipse) is centered at SVG
       y=240 with a tail dropping to y=480. Two viewBox/path tricks
       to align the bubble body's optical center with the dots in
       neighboring rows:

       1. Cropping the viewBox to "0 -224 512 448" so the ellipse
          center (y=240) sits at the viewBox vertical center
          (-224 + 224 = 0, with height 448 → centered at y=0).
       2. Translating the path up by 240 so what's drawn at SVG
          y=240 lives at viewBox y=0.

       Result: the SVG element's geometric center IS the ellipse
       center, regardless of the tail. Sized to match the dots
       (no overflow, no flex centering complications). -->
  <svg
    class="status-bubble"
    viewBox="-256 -224 512 448"
    width="10"
    height="10"
    fill="currentColor"
    aria-label={label}
    role="img"
  >
    <title>{label}</title>
    <path
      transform="translate(-256 -240)"
      d="M512 240c0 114.9-114.6 208-256 208c-37.1 0-72.3-6.4-104.1-17.9c-11.9 8.7-31.3 20.6-54.3 30.6C73.6 471.1 44.7 480 16 480c-6.5 0-12.3-3.9-14.8-9.9c-2.5-6-1.1-12.8 3.4-17.4c.4-.4 .8-.8 1.3-1.4c1.1-1.2 2.8-3.1 4.9-5.7c4.1-5 9.6-12.4 15.2-21.6c10-16.6 19.5-38.4 21.4-62.9C17.7 326.8 0 285.1 0 240C0 125.1 114.6 32 256 32s256 93.1 256 208z"
    />
  </svg>
{:else}
  <span
    class="status-dot status-dot--{status}"
    style:width="{size}px"
    style:height="{size}px"
    title={label}
    aria-label={label}
  ></span>
{/if}

<style>
  .status-dot {
    display: inline-block;
    border-radius: 50%;
    flex-shrink: 0;
    vertical-align: middle;
    box-sizing: border-box;
  }

  /* Working — file is being written right now. Filled green dot
     with a pulsing glow so it draws the eye. */
  .status-dot--working {
    background: var(--accent-green, #22c55e);
    animation: status-pulse 2.4s ease-in-out infinite;
    will-change: box-shadow;
  }

  /* Recently active but no positive "waiting" signal (other agents
     without classifier, or Claude mid-turn between writes). Smaller
     filled dot using a muted green so it sits below pulse and
     glyph in visual weight but stays readable. */
  .status-dot--idle {
    background: color-mix(
      in srgb,
      var(--accent-green, #22c55e) 55%,
      transparent
    );
    transform: scale(0.7);
  }

  .status-dot--stale {
    background: var(--accent-amber, #f59e0b);
  }

  .status-dot--unclean {
    background: var(--accent-red, #ef4444);
  }

  .status-dot--quiet {
    background: transparent;
  }

  /* Waiting on user input — the agent reached end_turn /
     task_complete and is parked, asking the user to respond.
     A small speech bubble, dimmed and slowly breathing. The
     pulse is opacity-only (no glow) so it reads as a calm "your
     turn" rather than competing with the working green's
     attention-grabbing halo. The viewBox/path transform in the
     SVG itself puts the bubble body's ellipse center at the
     SVG's geometric center, so vertical-align: middle aligns
     the body with the dots in adjacent rows. */
  .status-bubble {
    display: inline-block;
    flex-shrink: 0;
    vertical-align: middle;
    overflow: visible;
    color: var(--status-waiting, #a48a55);
    animation: icon-breathe 2.6s ease-in-out infinite;
    will-change: opacity;
  }

  @keyframes icon-breathe {
    0%,
    100% {
      opacity: 0.45;
    }
    50% {
      opacity: 0.9;
    }
  }

  @keyframes status-pulse {
    0%,
    100% {
      box-shadow: 0 0 0 0 transparent;
    }
    50% {
      box-shadow: 0 0 6px 3px
        color-mix(in srgb, var(--accent-green, #22c55e) 50%, transparent);
    }
  }
</style>
