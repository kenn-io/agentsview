import type { SearchBlockState } from "../search-block.svelte.js";

export interface FixtureProps extends SearchBlockState {
  blockKey: string;
  html: string;
}

export function reactiveProps(initial: FixtureProps): FixtureProps {
  const state = $state(initial);
  return state;
}
