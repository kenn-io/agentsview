<script lang="ts">
  import VirtualizerTest from './VirtualizerTest.svelte';

  type Options = { measureCacheKey: string; count: number };

  interface Props {
    type: 'element' | 'window';
    controller: {
        initialOptions: Options;
        updateOptions: (opts: Options) => void;
    };
    onInstanceChange: (inst: unknown) => void;
  }

  let { type, controller, onInstanceChange }: Props = $props();

  // Test scaffolding: read the initial options off the controller once,
  // then attach a mutator the test can call to drive option changes.
  // The controller is a stable per-test object; we don't expect the
  // prop reference itself to change during a test run.
  // svelte-ignore state_referenced_locally
  let options = $state(controller.initialOptions);

  // svelte-ignore state_referenced_locally
  controller.updateOptions = (newOpts: Options) => {
    options = newOpts;
  };
</script>

<VirtualizerTest {type} {options} {onInstanceChange} />
