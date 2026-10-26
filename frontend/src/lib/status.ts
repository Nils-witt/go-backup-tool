import type { RunState } from "../api/types";

export type StateCounts = Partial<Record<RunState, number>>;

// countStates tallies how many of states are in each RunState, for the
// dashboard's summary bars.
export function countStates(states: RunState[]): StateCounts {
  const counts: StateCounts = {};
  for (const s of states) counts[s] = (counts[s] ?? 0) + 1;
  return counts;
}

// Sourced tags a job or receiver snapshot with the instance it came from:
// instance is its name, unset while the dashboard shows only this one.
// remote marks one another instance pushed to this one (see /api/peers), so
// actions only its own dashboard can take (run, retry, file listing,
// downloads) are hidden for it. instanceUuid is that instance's server
// UUID, when known, which a remote target's server_uuid names.
export type Sourced<T> = T & { instance?: string; remote?: boolean; instanceUuid?: string };

// sourcedKey is a React key unique across instances.
export function sourcedKey(instance: string | undefined, id: string): string {
  return (instance ?? "") + "\u0000" + id;
}
