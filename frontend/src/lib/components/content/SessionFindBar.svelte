<script lang="ts">
  import { FindBar } from "@kenn-io/kit-ui";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { m } from "../../i18n/index.js";
</script>

{#if inSessionSearch.isOpen}
  <FindBar
    bind:query={inSessionSearch.query}
    matchCount={inSessionSearch.matches.length}
    currentIndex={inSessionSearch.currentMatchIndex}
    onnext={() => inSessionSearch.next()}
    onprev={() => inSessionSearch.prev()}
    onclose={() => inSessionSearch.close()}
    placeholder={m.session_find_placeholder()}
    matchCountLabel={m.session_find_match_count({
      current: "{current}",
      total: "{total}",
    })}
    noMatchesLabel={inSessionSearch.loading ? "…" : m.session_find_no_results()}
    ariaLabel={m.session_find_find_in_session()}
    inputAriaLabel={m.session_find_search_query()}
    previousLabel={m.session_find_previous_match()}
    nextLabel={m.session_find_next_match()}
    closeLabel={m.session_find_close()}
  />
{/if}
