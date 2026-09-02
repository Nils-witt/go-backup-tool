import type { DownloadEventJSON } from "../api/types";
import { Badge } from "./Badge";
import { fmtTime } from "../lib/format";

export function DownloadLogSection({ events }: { events: DownloadEventJSON[] }) {
  if (!events.length) return null;

  return (
    <div id="download-log-wrap">
      <h2 className="section-title">Download log</h2>
      <div className="card download-log-card">
        <table className="download-events">
          <thead>
            <tr>
              <th>Time</th>
              <th>Username</th>
              <th>Receiver</th>
              <th>File</th>
              <th>Result</th>
              <th>Remote address</th>
            </tr>
          </thead>
          <tbody>
            {events.map((ev, i) => (
              <tr key={i}>
                <td className="nowrap">{fmtTime(ev.at)}</td>
                <td>{ev.username || "(unknown)"}</td>
                <td className="nowrap">{ev.receiver_id}</td>
                <td>{ev.key}</td>
                <td className="nowrap">
                  {ev.success ? <Badge state="ok" label="success" /> : <Badge state="failed" label="failed" />}
                  {ev.detail ? <p className="err">{ev.detail}</p> : null}
                </td>
                <td>{ev.remote_addr}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
