import type { ServiceSessionDetail } from "../generated/index.js";

/** Session data with sidebar hydration state. */
export interface Session extends ServiceSessionDetail {
  is_teammate?: boolean;
  /** True until a sidebar index row has been hydrated. */
  is_index_only?: boolean;
}
