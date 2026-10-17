<script lang="ts">
  import { errorTelemetry } from "../error-telemetry.svelte.js";
  let state = $state({ visible: true, identity: "first", detail: "Failure" });
  export function update(visible: boolean, identity = state.identity, detail = state.detail) {
    Object.assign(state, { visible, identity, detail });
  }
</script>
{#if state.visible}
  <p {@attach errorTelemetry("search", state.identity)}>{state.detail}</p>
{/if}
