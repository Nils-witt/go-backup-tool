import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { JobSnapshot, LiveStatusMessage, ReceiverSnapshot } from "../api/types";

export interface LiveStatusState {
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
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
  const [state, setState] = useState<LiveStatusState>({ jobs: [], receivers: [], live: false });

  useEffect(() => {
    let stopped = false;
    let socket: WebSocket | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    let backoff = minBackoffMs;

    const scheduleReconnect = () => {
      if (stopped) return;
      setState((s) => (s.live ? { ...s, live: false } : s));
      retryTimer = setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, maxBackoffMs);
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
      if (stopped) return;

      const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(
        `${proto}//${window.location.host}/api/live?ticket=${encodeURIComponent(ticket)}`,
      );
      socket = ws;

      ws.onmessage = (ev: MessageEvent<string>) => {
        const msg = JSON.parse(ev.data) as LiveStatusMessage;
        if (msg.type !== "status") return;
        backoff = minBackoffMs;
        setState({ jobs: msg.jobs || [], receivers: msg.receivers || [], live: true });
      };
      ws.onclose = () => {
        if (socket === ws) socket = null;
        scheduleReconnect();
      };
    }

    void connect();

    return () => {
      stopped = true;
      clearTimeout(retryTimer);
      socket?.close();
    };
  }, []);

  return state;
}
