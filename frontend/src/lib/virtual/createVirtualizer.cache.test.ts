// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vite-plus/test';
import { mount, unmount, tick } from 'svelte';
// @ts-ignore
import CacheTestWrapper from './CacheTestWrapper.svelte';

interface MockInstance {
  options: Record<string, unknown>;
  itemSizeCache?: Map<string | number, number>;
  setOptions(opts: Record<string, unknown>): void;
  _willUpdate(): void;
}

type TestController = {
  initialOptions: { measureCacheKey: string; count: number };
  updateOptions: (
    opts: { measureCacheKey: string; count: number }
  ) => void;
};

const { createdInstances } = vi.hoisted(() => ({
  createdInstances: [] as MockInstance[],
}));

vi.mock('@tanstack/virtual-core', async () => {
  const original = await vi.importActual<
    typeof import('@tanstack/virtual-core')
  >('@tanstack/virtual-core');

  class MockVirtualizer implements MockInstance {
    options: Record<string, unknown>;
    itemSizeCache?: Map<string | number, number>;

    constructor(opts: Record<string, unknown>) {
      this.options = opts;
      createdInstances.push(this);
    }

    setOptions(opts: Record<string, unknown>) {
      this.options = opts;
    }

    _willUpdate() {}
  }

  return {
    ...original,
    Virtualizer: MockVirtualizer,
    observeElementOffset: vi.fn(),
    observeElementRect: vi.fn(),
    elementScroll: vi.fn(),
    observeWindowOffset: vi.fn(),
    observeWindowRect: vi.fn(),
    windowScroll: vi.fn(),
  };
});

describe('createVirtualizer cache invalidation', () => {
  let activeComponent: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    createdInstances.length = 0;
    vi.clearAllMocks();
  });

  afterEach(() => {
    if (activeComponent) {
      unmount(activeComponent);
      activeComponent = undefined;
    }
  });

  async function setupTest(
    type: 'element' | 'window',
    initialKey: string,
  ) {
    const controller: TestController = {
      initialOptions: { measureCacheKey: initialKey, count: 10 },
      updateOptions: () => {
        throw new Error('Not yet bound by component');
      },
    };

    activeComponent = mount(CacheTestWrapper, {
      target: document.body,
      props: { type, controller, onInstanceChange: vi.fn() },
    });

    await tick();

    expect(createdInstances).toHaveLength(1);
    const instance = createdInstances[0]!;

    // Simulate measurement accumulation
    instance.itemSizeCache = new Map([
      ['0', 50],
      ['1', 60],
    ]);

    return { controller, instance };
  }

  const types = ['element', 'window'] as const;

  types.forEach((type) => {
    describe(`${type} virtualizer`, () => {
      it('preserves cache when measureCacheKey stays the same', async () => {
        const { controller, instance } = await setupTest(
          type,
          'session-1',
        );

        controller.updateOptions({
          measureCacheKey: 'session-1',
          count: 20,
        });
        await tick();

        expect(createdInstances).toHaveLength(1);
        expect(instance.options.count).toBe(20);
        expect(instance.itemSizeCache?.get('0')).toBe(50);
        expect(instance.itemSizeCache?.get('1')).toBe(60);
      });

      it('clears cache when measureCacheKey changes', async () => {
        const { controller, instance } = await setupTest(
          type,
          'session-1',
        );

        controller.updateOptions({
          measureCacheKey: 'session-2',
          count: 10,
        });
        await tick();

        expect(createdInstances).toHaveLength(1);
        expect(instance.options.count).toBe(10);
        expect(instance.itemSizeCache).toBeDefined();
        expect(instance.itemSizeCache?.size).toBe(0);
      });
    });
  });
});
