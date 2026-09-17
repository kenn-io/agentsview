import type { DbTurnTiming as TurnTiming } from "../api/generated/index.js";

export function turnHasCategory(turn: TurnTiming, category: string): boolean {
  return turn.calls.some((call) => call.category === category);
}
