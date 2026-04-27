<script lang="ts">
  import { createVirtualizer, createWindowVirtualizer } from './createVirtualizer.svelte.js';

  interface Props {
    type: 'element' | 'window';
    options: any;
    onInstanceChange: (inst: any) => void;
  }

  let { type, options, onInstanceChange } = $props();

  // Test scaffolding: capture initial options into local state so
  // setOptions() can override from outside. The $effect below keeps
  // the local state in sync with prop changes.
  // svelte-ignore state_referenced_locally
  let currentOptions = $state(options);

  $effect(() => {
    currentOptions = options;
  });

  // Test scaffolding: type is selected once at mount; tests that need
  // to switch type remount the component.
  // svelte-ignore state_referenced_locally
  const virtualizer = type === 'window'
    ? createWindowVirtualizer(() => currentOptions)
    : createVirtualizer(() => currentOptions);

  $effect(() => {
    onInstanceChange(virtualizer.instance);
  });

  export function getVirtualizer() {
    return virtualizer;
  }

  export function setOptions(opts: any) {
    currentOptions = opts;
  }
</script>
