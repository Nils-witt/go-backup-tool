import type { JobSnapshot, ReceiverSnapshot, RunState, TargetSnapshot } from "../api/types";
import { sourcedKey, type Sourced } from "./status";

// The topology chart's three columns: jobs on the left, the servers their
// targets upload to in the middle, and the receivers remote servers deliver
// to on the right.
export type TopologyColumn = "job" | "server" | "receiver";

export interface TopologyNode {
  key: string;
  column: TopologyColumn;
  label: string;
  // detail is a short secondary line: a server's type, a receiver's path.
  detail?: string;
  source?: string;
  state: RunState;
  stale?: boolean;
  error?: string;
}

export interface TopologyEdge {
  key: string;
  from: string;
  to: string;
  state: RunState;
  // label lists the bucket(s) the connection carries.
  label: string;
  // ambiguous marks a server → receiver edge guessed among several
  // instances with a receiver of that id.
  ambiguous?: boolean;
}

export interface Topology {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
}

// Most severe first: an edge or server carrying several targets shows the
// worst of their states.
const SEVERITY: RunState[] = ["failed", "incomplete", "running", "ok", "idle"];

export function worstState(a: RunState, b: RunState): RunState {
  return SEVERITY.indexOf(a) <= SEVERITY.indexOf(b) ? a : b;
}

function nodeKey(column: TopologyColumn, source: string | undefined, id: string): string {
  return column + ":" + sourcedKey(source, id);
}

// receiversFor picks which of candidates — every receiver in view with the
// remote target t's bucket as its id — t delivers to. A target whose server
// names its destination's server_uuid connects only to that instance's
// receiver; receivers on instances whose UUID is known to differ are never
// it. Without a match by UUID (no server_uuid configured, or the instance's
// UUID couldn't be read), the remaining candidates are guessed among,
// preferring one on an instance named like the server.
function receiversFor(
  t: TargetSnapshot,
  candidates: Sourced<ReceiverSnapshot>[],
): Sourced<ReceiverSnapshot>[] {
  const uuid = t.server_uuid?.toLowerCase();
  if (uuid) {
    const pinned = candidates.filter((r) => r.instanceUuid?.toLowerCase() === uuid);
    if (pinned.length) return pinned;
    candidates = candidates.filter((r) => !r.instanceUuid);
  }

  if (candidates.length > 1) {
    const named = candidates.filter(
      (r) => r.source && r.source.toLowerCase() === t.server.toLowerCase(),
    );
    if (named.length) return named;
  }

  return candidates;
}

// buildTopology derives the chart's graph from the dashboard's merged job
// and receiver snapshots. A remote target's bucket is the id of the
// receiver it delivers to, so a remote server connects to the receiver in
// view with that id on the instance its server_uuid names (see
// receiversFor), else to every candidate, marked ambiguous when several.
export function buildTopology(
  jobs: Sourced<JobSnapshot>[],
  receivers: Sourced<ReceiverSnapshot>[],
): Topology {
  const nodes = new Map<string, TopologyNode>();
  const edges = new Map<string, TopologyEdge>();

  const addEdge = (
    from: string,
    to: string,
    state: RunState,
    bucket: string,
    ambiguous = false,
  ) => {
    const key = from + "\u0001" + to;
    const e = edges.get(key);
    if (!e) {
      edges.set(key, { key, from, to, state, label: bucket, ambiguous });
      return;
    }
    e.state = worstState(e.state, state);
    if (!e.label.split(", ").includes(bucket)) e.label += ", " + bucket;
  };

  const receiversById = new Map<string, Sourced<ReceiverSnapshot>[]>();
  for (const r of receivers) {
    const list = receiversById.get(r.id);
    if (list) list.push(r);
    else receiversById.set(r.id, [r]);
  }

  // Receiver nodes are added in the order their senders reach them, so the
  // columns line up and edges cross as little as possible; receivers
  // nothing in view sends to follow at the end.
  const receiverNodes = new Map<string, TopologyNode>();
  const addReceiver = (r: Sourced<ReceiverSnapshot>): string => {
    const key = nodeKey("receiver", r.source, r.id);
    if (!receiverNodes.has(key)) {
      receiverNodes.set(key, {
        key,
        column: "receiver",
        label: r.id,
        detail: r.path,
        source: r.source,
        state: r.stale ? "incomplete" : r.state,
        stale: r.stale,
        error: r.error,
      });
    }
    return key;
  };

  for (const j of jobs) {
    const jobKey = nodeKey("job", j.source, j.name);
    nodes.set(jobKey, {
      key: jobKey,
      column: "job",
      label: j.name,
      detail: j.interval ? "every " + j.interval : undefined,
      source: j.source,
      state: j.state,
      error: j.error,
    });

    for (const t of j.targets || []) {
      // Servers are per instance: two instances may each name one "nas".
      const serverKey = nodeKey("server", j.source, t.server);
      const server = nodes.get(serverKey);
      if (server) server.state = worstState(server.state, t.state);
      else
        nodes.set(serverKey, {
          key: serverKey,
          column: "server",
          label: t.server,
          detail: t.kind,
          source: j.source,
          state: t.state,
        });
      addEdge(jobKey, serverKey, t.state, t.bucket);

      if (t.kind !== "remote") continue;

      const matches = receiversFor(t, receiversById.get(t.bucket) ?? []);
      for (const r of matches) {
        addEdge(serverKey, addReceiver(r), t.state, t.bucket, matches.length > 1);
      }
    }
  }

  for (const r of receivers) addReceiver(r);

  return { nodes: [...nodes.values(), ...receiverNodes.values()], edges: [...edges.values()] };
}

// connectedKeys returns every node and edge on a path through key: its
// upstream senders and downstream destinations, for hover highlighting.
export function connectedKeys(topology: Topology, key: string): Set<string> {
  const seen = new Set<string>([key]);
  const walk = (start: string, forward: boolean) => {
    const queue = [start];
    while (queue.length) {
      const k = queue.shift()!;
      for (const e of topology.edges) {
        const [here, there] = forward ? [e.from, e.to] : [e.to, e.from];
        if (here !== k) continue;
        seen.add(e.key);
        if (!seen.has(there)) {
          seen.add(there);
          queue.push(there);
        }
      }
    }
  };

  walk(key, true);
  walk(key, false);

  return seen;
}
