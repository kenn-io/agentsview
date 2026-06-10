// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vite-plus/test';
import { mount, unmount, tick } from 'svelte';
// @ts-ignore
import VirtualizerTest from './VirtualizerTest.svelte';

const ASYNC_UPDATE_DELAY_MS = 100;

type MockOptions = Record<string, unknown>;

const { lastOptions, lastInstance } = vi.hoisted(() => ({
  lastOptions: { value: undefined as MockOptions | undefined },
  lastInstance: { value: undefined as Record<string, unknown> | undefined }
}));

vi.mock('@tanstack/virtual-core', async () => {
  const original = await vi.importActual<typeof import('@tanstack/virtual-core')>('@tanstack/virtual-core');
  return {
    ...original,
    Virtualizer: class {
      options: MockOptions;
      scrollOffset: number | undefined = undefined;
      constructor(opts: MockOptions) {
        this.options = opts;
        lastOptions.value = opts;
        lastInstance.value = this as unknown as Record<string, unknown>;
      }
      setOptions(opts: MockOptions) {
        this.options = opts;
        lastOptions.value = opts;
      }
      _willUpdate() {}
    },
    observeElementOffset: vi.fn(),
    observeElementRect: vi.fn(),
    elementScroll: vi.fn(),
    observeWindowOffset: vi.fn(),
    observeWindowRect: vi.fn(),
    windowScroll: vi.fn(),
  };
});

describe('initialOffset semantics', () => {
  beforeEach(() => {
    lastOptions.value = undefined;
    lastInstance.value = undefined;
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('element virtualizer uses scrollTop on first mount', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');
    const scrollDiv = document.createElement('div');
    Object.defineProperty(scrollDiv, 'scrollTop', {
      value: 200,
      writable: false,
    });

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'element',
        options: {
          count: 10,
          getScrollElement: () => scrollDiv,
          estimateSize: () => 50,
          initialOffset: 999,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(200);

    unmount(component);
  });

  it('element virtualizer falls back to 0 with null scroll element', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'element',
        options: {
          count: 10,
          getScrollElement: () => null,
          estimateSize: () => 50,
          initialOffset: 999,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(0);

    unmount(component);
  });

  it('window virtualizer uses 0 on first mount', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'window',
        options: {
          count: 20,
          estimateSize: () => 50,
          initialOffset: 999,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(0);

    unmount(component);
  });

  it('element virtualizer ignores user initialOffset with scrollTop=0', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');
    const scrollDiv = document.createElement('div');
    Object.defineProperty(scrollDiv, 'scrollTop', {
      value: 0,
      writable: false,
    });

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'element',
        options: {
          count: 10,
          getScrollElement: () => scrollDiv,
          estimateSize: () => 50,
          initialOffset: 500,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(0);

    unmount(component);
  });

  it('window virtualizer ignores user initialOffset', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'window',
        options: {
          count: 20,
          estimateSize: () => 50,
          initialOffset: 500,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(0);

    unmount(component);
  });

  it('update path prefers instance.scrollOffset over wrapper initialOffset', async () => {
    const onInstanceChange = vi.fn();
    const container = document.createElement('div');
    const scrollDiv = document.createElement('div');
    Object.defineProperty(scrollDiv, 'scrollTop', {
      value: 0,
      writable: false,
    });

    const component = mount(VirtualizerTest, {
      target: container,
      props: {
        type: 'element',
        options: {
          count: 10,
          getScrollElement: () => scrollDiv,
          estimateSize: () => 50,
        },
        onInstanceChange,
      },
    });

    await tick();
    expect(lastOptions.value).toBeDefined();
    expect(lastOptions.value!.initialOffset).toBe(0);

    // Simulate the instance having scrolled to offset 150
    lastInstance.value!.scrollOffset = 150;

    // Trigger an options update via setOptions on the test harness,
    // which re-runs the $effect and hits the update path
    component.setOptions({
      count: 15,
      getScrollElement: () => scrollDiv,
      estimateSize: (): number => 50,
    });
    await tick();

    expect(lastOptions.value!.initialOffset).toBe(150);

    unmount(component);
  });
});

describe('createVirtualizer reactivity', () => {
  beforeEach(() => {
    lastOptions.value = undefined;
    lastInstance.value = undefined;
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it.each([
    {
      type: 'element' as const,
      options: {
        count: 10,
        getScrollElement: () => document.createElement('div'),
        estimateSize: (): number => 50,
      },
    },
    {
      type: 'window' as const,
      options: {
        count: 20,
        estimateSize: (): number => 50,
      },
    },
  ])(
    'updates when onChange fires with same reference ($type virtualizer)',
    async ({ type, options }) => {
      const onInstanceChange = vi.fn();
      const container = document.createElement('div');

      const component = mount(VirtualizerTest, {
        target: container,
        props: { type, options, onInstanceChange },
      });

      await tick();

      expect(onInstanceChange).toHaveBeenCalledTimes(1);
      expect(lastOptions.value).toBeDefined();

      const { onChange } = lastOptions.value!;
      expect(typeof onChange).toBe('function');

      const firstInstance = onInstanceChange.mock.calls[0]![0];
      expect(component.getVirtualizer().instance).toBe(firstInstance);

      const rawInstance = lastInstance.value;
      expect(rawInstance).toBe(firstInstance);

      // Mutate raw instance to verify same object reference
      (rawInstance as Record<string, unknown>)._test_mutation = 'updated';

      // 1. Sync update (onChange(..., false))
      (onChange as (inst: unknown, async: boolean) => void)(rawInstance, false);
      await tick();
      vi.advanceTimersByTime(ASYNC_UPDATE_DELAY_MS);
      await tick();

      expect(onInstanceChange).toHaveBeenCalledTimes(2);
      const receivedSync = onInstanceChange.mock.calls[1]![0];
      expect(receivedSync).toBe(rawInstance);
      expect(receivedSync._test_mutation).toBe('updated');
      expect(component.getVirtualizer().instance).toBe(rawInstance);

      // 2. Async update (onChange(..., true))
      (onChange as (inst: unknown, async: boolean) => void)(rawInstance, true);

      // Should not have updated yet (setTimeout is pending)
      await tick();
      expect(onInstanceChange).toHaveBeenCalledTimes(2);

      // Advance timers to trigger the queued update
      vi.advanceTimersByTime(ASYNC_UPDATE_DELAY_MS);
      await tick();

      expect(onInstanceChange).toHaveBeenCalledTimes(3);
      const receivedAsync = onInstanceChange.mock.calls[2]![0];
      expect(receivedAsync).toBe(rawInstance);
      expect(receivedAsync._test_mutation).toBe('updated');

      unmount(component);
    },
  );
});
