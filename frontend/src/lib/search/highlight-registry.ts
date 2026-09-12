/** Own only the two in-session CSS highlights, never the page-wide registry. */
export const FIND_HIGHLIGHT = "av-find";
export const CURRENT_HIGHLIGHT = "av-find-current";

interface HighlightSet {
  priority: number;
  add(range: AbstractRange): unknown;
  delete(range: AbstractRange): boolean;
}

export interface HighlightHost {
  CSS?: {
    highlights?: {
      get(name: string): HighlightSet | undefined;
      set(name: string, value: HighlightSet): unknown;
      delete(name: string): boolean;
    };
  };
  Highlight?: new (...ranges: AbstractRange[]) => HighlightSet;
}

interface OwnedRanges {
  all: readonly AbstractRange[];
  current: readonly AbstractRange[];
}

export function createHighlightRegistry(
  host: HighlightHost = globalThis as unknown as HighlightHost,
) {
  const owners = new Map<object, OwnedRanges>();
  let all: HighlightSet | undefined;
  let current: HighlightSet | undefined;

  function remove(owner: object): void {
    const ranges = owners.get(owner);
    if (!ranges) return;
    ranges.all.forEach((range) => all?.delete(range));
    ranges.current.forEach((range) => current?.delete(range));
    owners.delete(owner);
    if (owners.size) return;
    const registry = host.CSS?.highlights;
    if (all && registry?.get(FIND_HIGHLIGHT) === all) registry.delete(FIND_HIGHLIGHT);
    if (current && registry?.get(CURRENT_HIGHLIGHT) === current) registry.delete(CURRENT_HIGHLIGHT);
    all = undefined;
    current = undefined;
  }

  return {
    get supported(): boolean {
      return typeof host.Highlight === "function" && !!host.CSS?.highlights;
    },
    set(
      owner: object,
      ranges: readonly AbstractRange[],
      selected: readonly AbstractRange[] = [],
    ): void {
      remove(owner);
      const registry = host.CSS?.highlights;
      const Constructor = host.Highlight;
      if (!Constructor || !registry || !ranges.length) return;
      if (!all || !current) {
        all = new Constructor();
        current = new Constructor();
        current.priority = 1;
        registry.set(FIND_HIGHLIGHT, all);
        registry.set(CURRENT_HIGHLIGHT, current);
      }
      ranges.forEach((range) => all!.add(range));
      selected.forEach((range) => current!.add(range));
      owners.set(owner, { all: ranges, current: selected });
    },
    remove,
  };
}

export const highlightRegistry = createHighlightRegistry();
