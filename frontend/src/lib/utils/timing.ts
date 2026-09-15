import type { TurnTiming } from "../api/types/timing.js";

export function turnHasCategory(turn: TurnTiming, category: string): boolean {
  return turn.calls.some((call) => call.category === category);
}
