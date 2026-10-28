<script lang="ts">
  import { m } from "../../i18n/index.js";
</script>

<!-- A static transcript sample drawn from the same role tokens as the real
     message components, so a color or pattern change shows here at once. -->
<div class="preview" aria-label={m.appearance_message_colors_preview()} role="group">
  <div class="sample message user">
    <div class="sample-header">
      <span class="role-icon">U</span>
      <span class="role-label">{m.message_content_role_user()}</span>
    </div>
    <p>{m.appearance_message_colors_preview_user()}</p>
  </div>
  <div class="sample message assistant">
    <div class="sample-header">
      <span class="role-icon">A</span>
      <span class="role-label">{m.message_content_role_assistant()}</span>
    </div>
    <div class="thinking">{m.thinking_block_label()}</div>
    <div class="tool"><span class="tool-label">Bash</span> npm test</div>
    <p>{m.appearance_message_colors_preview_assistant()}</p>
    <pre class="code"><code>expect(date).toBe("2026-01-01T00:00:00Z");</code></pre>
  </div>
</div>

<style>
  .preview {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    width: 100%;
    max-width: 520px;
    font-size: var(--font-size-sm);
  }

  .sample {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4) var(--space-5);
    border-radius: 0 var(--radius-md) var(--radius-md) 0;
    color: var(--text-primary);
  }

  .user {
    background: var(--user-bg);
    border-left: var(--role-user-bar-width) var(--role-user-bar-style) var(--role-user-accent);
    --sample-accent: var(--role-user-accent);
    --sample-accent-foreground: var(--role-user-accent-foreground);
    --sample-ink: var(--role-user-ink);
  }

  .assistant {
    background: var(--assistant-bg);
    border-left: var(--role-assistant-bar-width) var(--role-assistant-bar-style)
      var(--role-assistant-accent);
    --sample-accent: var(--role-assistant-accent);
    --sample-accent-foreground: var(--role-assistant-accent-foreground);
    --sample-ink: var(--role-assistant-ink);
  }

  .sample-header {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .role-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 18px;
    height: 18px;
    border-radius: 50%;
    background: var(--sample-accent);
    color: var(--sample-accent-foreground);
    font-size: var(--font-size-2xs);
    font-weight: 700;
  }

  .role-label {
    color: var(--sample-ink);
    font-weight: 600;
  }

  .thinking,
  .tool {
    padding: var(--space-2) var(--space-4);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
    font-size: var(--font-size-xs);
  }

  .thinking {
    background: var(--thinking-bg);
    border-left: var(--role-thinking-bar-width) var(--role-thinking-bar-style)
      var(--role-thinking-accent);
    color: var(--role-thinking-ink);
    font-weight: 600;
  }

  .tool {
    background: var(--tool-bg);
    border-left: var(--role-tool-bar-width) var(--role-tool-bar-style) var(--role-tool-accent);
    color: var(--text-secondary);
    font-family: var(--font-mono);
  }

  .tool-label {
    color: var(--role-tool-ink);
    font-weight: 500;
  }

  .code {
    margin: 0;
    padding: var(--space-4) var(--space-5);
    background: var(--code-bg);
    color: var(--code-text);
    border: var(--role-code-frame-width) var(--role-code-frame-style) var(--role-code-accent);
    border-radius: var(--radius-md);
    font-size: var(--font-size-xs);
    white-space: pre-wrap;
  }
</style>
