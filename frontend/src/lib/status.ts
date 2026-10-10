import type { RunState } from "../api/types";

export type StateCounts = Partial<Record<RunState, number>>;

// countStates tallies how many of states are in each RunState, for the
// dashboard's summary bars.
export function countStates(states: RunState[]): StateCounts {
  const counts: StateCounts = {};
  for (const s of states) counts[s] = (counts[s] ?? 0) + 1;
  return counts;
}

// Sourced tags a job or receiver snapshot with the instance it came from,
// for a dashboard merging this instance with remote backends (see
// lib/remoteBackends). source is unset when no remote backend is configured;
// remote marks one read with a view-only API token, so actions that need
// more (retry, file listing, downloads) are hidden for it. instanceUuid is
// that instance's server UUID, when known, which a remote target's
// server_uuid names.
export type Sourced<T> = T & { source?: string; remote?: boolean; instanceUuid?: string };

// sourcedKey is a React key unique across instances.
export function sourcedKey(source: string | undefined, id: string): string {
  return (source ?? "") + "\u0000" + id;
}
