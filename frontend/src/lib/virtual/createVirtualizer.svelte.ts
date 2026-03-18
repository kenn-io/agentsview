import {
  Virtualizer,
  type VirtualizerOptions,
  observeElementOffset,
  observeElementRect,
  elementScroll,
  observeWindowOffset,
  observeWindowRect,
  windowScroll,
} from "@tanstack/virtual-core";

type PartialKeys<T, K extends keyof T> = Omit<T, K> &
  Partial<Pick<T, K>>;

type ElementOpts = PartialKeys<
  VirtualizerOptions<HTMLElement, HTMLElement>,
  | "observeElementOffset"
  | "observeElementRect"
  | "scrollToFn"
> & { measureCacheKey?: unknown };

type WindowOpts = PartialKeys<
  VirtualizerOptions<Window, HTMLElement>,
  | "observeElementOffset"
  | "observeElementRect"
  | "scrollToFn"
  | "getScrollElement"
> & { measureCacheKey?: unknown };

type BaseOpts<
  TScroll extends Element | Window,
  TItem extends Element,
> = VirtualizerOptions<TScroll, TItem> & {
  measureCacheKey?: unknown;
};

function createBaseVirtualizer<
  TScroll extends Element | Window,
  TItem extends Element,
>(
  optsFn: () => BaseOpts<TScroll, TItem>,
  postUpdate?: (
    instance: Virtualizer<TScroll, TItem>,
    opts: BaseOpts<TScroll, TItem>,
    reset: boolean,
  ) => void,
) {
  let instance:
    | Virtualizer<TScroll, TItem>
    | undefined = undefined;
  let notifyPending = false;
  let lastMeasureCacheKey: unknown = undefined;
  let cacheKeyChanged = false;
  let _version = $state(0);

  function bumpVersion() {
    if (notifyPending) return;
    notifyPending = true;
    setTimeout(() => {
      notifyPending = false;
      _version++;
    }, 0);
  }

  $effect(() => {
    const opts = optsFn();
    const willReset =
      opts.measureCacheKey !== lastMeasureCacheKey &&
      instance !== undefined;
    const resolvedOpts: VirtualizerOptions<TScroll, TItem> = {
      ...opts,
      initialOffset: willReset
        ? 0
        : (instance?.scrollOffset ?? opts.initialOffset),
      onChange: (
        vInst: Virtualizer<TScroll, TItem>,
        sync: boolean,
      ) => {
        instance = vInst;
        if (sync) {
          bumpVersion();
        } else {
          _version++;
        }
        opts.onChange?.(vInst, sync);
      },
    };

    if (!instance) {
      const v = new Virtualizer(resolvedOpts);
      instance = v;
      lastMeasureCacheKey = opts.measureCacheKey;
      v._willUpdate();
      return () => {
        v._willUpdate();
      };
    }

    cacheKeyChanged = false;
    if (opts.measureCacheKey !== lastMeasureCacheKey) {
      // @ts-expect-error accessing private itemSizeCache
      instance.itemSizeCache = new Map();
      cacheKeyChanged = true;
    }
    lastMeasureCacheKey = opts.measureCacheKey;

    instance.setOptions(resolvedOpts);
    instance._willUpdate();

    postUpdate?.(instance, opts, cacheKeyChanged);

    return () => {
      instance?._willUpdate();
    };
  });

  return {
    get instance() {
      _version;
      return instance;
    },
  };
}

export function createVirtualizer(
  optsFn: () => ElementOpts,
) {
  return createBaseVirtualizer<HTMLElement, HTMLElement>(
    () => {
      const opts = optsFn();
      const scrollEl = opts.getScrollElement?.() ?? null;
      return {
        observeElementOffset,
        observeElementRect,
        scrollToFn: elementScroll,
        ...opts,
        initialOffset: scrollEl?.scrollTop ?? 0,
      };
    },
    (instance, opts, reset) => {
      const scrollEl = opts.getScrollElement?.() ?? null;
      if (!scrollEl) return;
      if (reset) {
        scrollEl.scrollTop = 0;
        return;
      }
      // Don't override an active scrollToIndex reconcile loop.
      // scrollState.index is non-null while TanStack is iterating
      // toward the target index. Calling scrollToOffset here would
      // reset scrollState.index to null, breaking the loop and
      // leaving the viewport at the wrong position (observed as
      // pinned-message navigation stopping mid-scroll in ascending
      // sort when the target item is not yet rendered).
      // scrollState is typed private but is a plain public field at runtime.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      if ((instance as any).scrollState?.index != null) return;
      if (scrollEl.scrollTop > 0) {
        instance.scrollToOffset(scrollEl.scrollTop);
      }
    },
  );
}

export function createWindowVirtualizer(
  optsFn: () => WindowOpts,
) {
  return createBaseVirtualizer<Window, HTMLElement>(() => ({
    observeElementOffset: observeWindowOffset,
    observeElementRect: observeWindowRect,
    scrollToFn: windowScroll,
    getScrollElement: () => window,
    ...optsFn(),
    initialOffset: 0,
  }));
}
