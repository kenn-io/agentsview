<script lang="ts">
  import { Toggle } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { yokedDates } from "../../stores/yokedDates.svelte.js";
</script>

<Toggle
  checked={yokedDates.enabled}
  onchange={(checked) => yokedDates.setEnabled(checked)}
  label={m.settings_date_ranges_link()}
/>
