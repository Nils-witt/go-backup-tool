import type { RunState } from "../api/types";

export type StateCounts = Partial<Record<RunState, number>>;

// countStates tallies how many of states are in each RunState, for the
// dashboard's summary bars.
export function countStates(states: RunState[]): StateCounts {
  const counts: StateCounts = {};
  for (const s of states) counts[s] = (counts[s] ?? 0) + 1;
  return counts;
}
