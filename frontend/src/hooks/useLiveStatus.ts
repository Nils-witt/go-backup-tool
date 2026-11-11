import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { JobSnapshot, LiveStatusMessage, PeerSnapshot, ReceiverSnapshot } from "../api/types";

export interface LiveStatusState {
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
  // peers are the other instances that push their status to this one.
  peers: PeerSnapshot[];
  // live is true while the socket is open and has delivered at least one
  // message; pages fall back to polling (see usePoll's enabled) otherwise.
  live: boolean;
}

const minBackoffMs = 1000;
const maxBackoffMs = 30000;

// useLiveStatus follows the /api/live WebSocket for as long as the calling
// component stays mounted. A browser can't attach the bearer token to a
// WebSocket handshake, so each connection first mints a one-time ticket
// through the authenticated API (which also renews an expired token). A
// dropped connection is retried with exponential backoff, minting a fresh
// ticket every time.
export function useLiveStatus(): LiveStatusState {
  const [state, setState] = useState<LiveStatusState>({
    jobs: [],
    receivers: [],
    peers: [],
    live: false,
  });

  useEffect(() => {
    // Mutable connection state lives in one object: closures mutate its
    // fields instead of reassigning locals (oxlint's react/immutability).
    const conn: {
      stopped: boolean;
      socket: WebSocket | null;
      retryTimer: ReturnType<typeof setTimeout> | undefined;
      backoff: number;
      // The last message's raw text: the server re-sends the full status on
      // every ping, and an identical one shouldn't re-render the dashboard.
      lastData: string | null;
    } = {
      stopped: false,
      socket: null,
      retryTimer: undefined,
      backoff: minBackoffMs,
      lastData: null,
    };

    const scheduleReconnect = () => {
      if (conn.stopped) return;
      conn.lastData = null;
      setState((s) => (s.live ? { ...s, live: false } : s));
      conn.retryTimer = setTimeout(connect, conn.backoff);
      conn.backoff = Math.min(conn.backoff * 2, maxBackoffMs);
    };

    async function connect() {
      let ticket: string;
      try {
        ({ ticket } = await apiFetchJSON<{ ticket: string }>("/api/live/ticket", {
          method: "POST",
        }));
      } catch {
        scheduleReconnect();
        return;
      }
      if (conn.stopped) return;

      const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(
        `${proto}//${window.location.host}/api/live?ticket=${encodeURIComponent(ticket)}`,
      );
      conn.socket = ws;

      ws.onmessage = (ev: MessageEvent<string>) => {
        if (ev.data === conn.lastData) return;
        const msg = JSON.parse(ev.data) as LiveStatusMessage;
        if (msg.type !== "status") return;
        conn.backoff = minBackoffMs;
        conn.lastData = ev.data;
        setState({
          jobs: msg.jobs || [],
          receivers: msg.receivers || [],
          peers: msg.peers || [],
          live: true,
        });
      };
      ws.onclose = () => {
        if (conn.socket === ws) conn.socket = null;
        scheduleReconnect();
      };
    }

    void connect();

    return () => {
      conn.stopped = true;
      clearTimeout(conn.retryTimer);
      conn.socket?.close();
    };
  }, []);

  return state;
}
