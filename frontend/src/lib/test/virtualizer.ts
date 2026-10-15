import {
  observeElementOffset,
  type Virtualizer,
  type VirtualizerOptions,
  // kit-ui-check-ignore: test fixture drives the real TanStack core that the local wrapper uses.
} from "@tanstack/virtual-core";

export function realVirtualizer(
  VirtualizerClass: typeof Virtualizer,
  options: {
    count: number;
    height?: number;
    onChange?: VirtualizerOptions<HTMLElement, HTMLElement>["onChange"];
  },
) {
  const element = document.createElement("div");
  const height = options.height ?? 100;
  let scrollTop = 0;
  Object.defineProperties(element, {
    scrollHeight: { get: () => instance.getTotalSize() },
    clientHeight: { value: height },
    scrollTop: {
      get: () => scrollTop,
      set: (offset: number) => {
        if (offset === scrollTop) return;
        scrollTop = offset;
        element.dispatchEvent(new Event("scroll"));
      },
    },
  });
  const sizes = new Map<string | number | bigint, number>();
  const instance = new VirtualizerClass<HTMLElement, HTMLElement>({
    count: options.count,
    getScrollElement: () => element,
    estimateSize: () => 100,
    measureElement: (row, _, virtualizer) =>
      sizes.get(virtualizer.options.getItemKey(Number(row.dataset.index))) ?? 100,
    observeElementRect: (_, callback) => callback({ width: 400, height }),
    observeElementOffset: (virtualizer, callback) => {
      callback(element.scrollTop, false);
      return observeElementOffset(virtualizer, callback);
    },
    scrollToFn: (offset, { adjustments }) => {
      element.scrollTop = offset + (adjustments ?? 0);
    },
    onChange: options.onChange,
  });
  const cleanup = instance._didMount();
  instance._willUpdate();
  return {
    instance,
    element,
    cleanup,
    measureRow: (index: number, size: number) => {
      sizes.set(instance.options.getItemKey(index), size);
      instance.resizeItem(index, size);
    },
  };
}
