<script lang="ts">
  import SettingsSection from "./SettingsSection.svelte";
  import { settings } from "../../stores/settings.svelte.js";

  const AGENT_LABELS: Record<string, string> = {
    claude: "Claude Code",
    codex: "Codex",
    copilot: "Copilot",
    gemini: "Gemini",
    opencode: "OpenCode",
    cursor: "Cursor",
    amp: "Amp",
    iflow: "iFlow",
    "vscode-copilot": "VSCode Copilot",
    pi: "Pi",
    openclaw: "OpenClaw",
  };
</script>

<SettingsSection
  title="Agent Directories"
  description="Directories scanned for session data. Configured via environment variables or config file."
>
  <div class="dir-list">
    {#each Object.entries(settings.agentDirs) as [agent, dirs]}
      <div class="dir-row">
        <span class="dir-agent">{AGENT_LABELS[agent] ?? agent}</span>
        <div class="dir-paths">
          {#if dirs.length === 0}
            <span class="dir-none">Not configured</span>
          {:else}
            {#each dirs as dir}
              <code class="dir-path">{dir}</code>
            {/each}
          {/if}
        </div>
      </div>
    {/each}
  </div>
</SettingsSection>

<style>
  .dir-list {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .dir-row {
    display: flex;
    align-items: baseline;
    gap: 12px;
  }

  .dir-agent {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
    min-width: 110px;
    flex-shrink: 0;
  }

  .dir-paths {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .dir-path {
    font-size: 11px;
    color: var(--text-muted);
    word-break: break-all;
  }

  .dir-none {
    font-size: 11px;
    color: var(--text-muted);
    font-style: italic;
  }
</style>
