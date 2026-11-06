import type {
  DbSessionTiming as SessionTiming,
  DbTurnTiming as TurnTiming,
} from "../api/generated/index.js";

export function turnHasCategory(turn: TurnTiming, category: string): boolean {
  return turn.calls.some((call) => call.category === category);
}

/**
 * Whether a call list has anything to show in its bar and duration columns:
 * at least one call with a measured duration, or a running turn whose live
 * call shows elapsed time. When false, the list drops both columns.
 */
export function callsHaveTiming(timing: SessionTiming): boolean {
  return (
    !!timing.running ||
    timing.turns.some((turn) => turn.calls.some((call) => call.duration_ms != null))
  );
}
