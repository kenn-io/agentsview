import type {
  DbSessionTiming as SessionTiming,
  DbTurnTiming as TurnTiming,
} from "../api/generated/index.js";

export function turnHasCategory(turn: TurnTiming, category: string): boolean {
  return turn.calls.some((call) => call.category === category);
}

/**
 * Whether a call list has anything to show in its bar and duration columns:
 * at least one call with a measured duration, or a live call showing its
 * elapsed time. A live call is the last call of the last turn while the
 * session runs and that turn has no duration yet, as the call lists render
 * it. When false, the list drops both columns.
 */
export function callsHaveTiming(timing: SessionTiming): boolean {
  const last = timing.turns.at(-1);
  const hasLiveCall =
    !!timing.running && last != null && last.duration_ms == null && last.calls.length > 0;
  return (
    hasLiveCall ||
    timing.turns.some((turn) => turn.calls.some((call) => call.duration_ms != null))
  );
}
